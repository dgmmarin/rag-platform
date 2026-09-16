# ISSUE-0063: Make ingest embed concurrency/batch tunable for slow self-hosted providers

**Type:** Feature · **Status:** Done · **Story:** EPIC-05 (ingestion) · **Traces:** SPEC-05 §4, NFR-REL-04

## Context
The embed client (`internal/ingest/embed`) defaults to **4 batches in flight × 96 texts/batch** against
a **60s** per-request timeout — tuned for a fast cloud provider. Against a **self-hosted CPU
endpoint** (TEI or an Ollama OpenAI-compatible endpoint), which serves requests roughly serially, a
full-size batch under 4-way concurrency exceeds 60s: measured **77–78s** for a 96-text batch at
concurrency 4 on CPU. Those requests time out, and after `DefaultBreakerThreshold` (5) consecutive
failures the circuit breaker opens and the sync ingests nothing.

## Change
Expose the two levers via config so a deployment can throttle the ingest embedder to fit its
provider's throughput. Both default to 0 = the embed package's existing defaults, so cloud
deployments are unchanged.
- **`internal/config/config.go`** — `EmbeddingMaxConcurrency` / `EmbeddingMaxBatchTexts`, read from
  `EMBEDDING_MAX_CONCURRENCY` / `EMBEDDING_MAX_BATCH_TEXTS` (validated non-negative integers).
- **`internal/worker/embedder.go`** — `KeyedEmbedderFactory` gains `Concurrency` / `MaxBatchTexts`,
  passed into `embed.Config`.
- **`internal/cli/worker.go`** — wires the two config values into the factory.

Example for a CPU Ollama endpoint (nomic-embed-text): `EMBEDDING_MAX_CONCURRENCY=1`,
`EMBEDDING_MAX_BATCH_TEXTS=32` (a 32-text batch is ~6.5s warm — comfortably inside the 60s timeout).

## Note
This is a robustness improvement, independent of a **separate operator gotcha** found at the same
time: `EMBEDDING_BASE_URL` must be the provider ORIGIN **without** a trailing `/v1` — the
`openai`-compatible provider appends `/v1/embeddings` itself. Setting
`http://host:11434/v1` yields `…/v1/v1/embeddings` → 404. Correct value: `http://host:11434`.

## Verification
- `go build ./...`: **PASS**; `go test ./internal/config/ ./internal/worker/`: **PASS**.
- End-to-end: with a corrected base URL + `EMBEDDING_MAX_CONCURRENCY=1`/`EMBEDDING_MAX_BATCH_TEXTS=32`,
  the acme web-crawl source ingests documents and chunks via a local Ollama `nomic-embed-text`
  (768-dim) endpoint — stored chunks carry real page text and non-null 768-dim vectors.
