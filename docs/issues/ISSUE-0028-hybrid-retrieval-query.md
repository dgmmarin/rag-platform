# ISSUE-0028: Hybrid retrieval query

**Type:** Feature · **Status:** Done · **Story:** STORY-08.1 · **Traces:** FR-RET-01/02/08, ADR-0007, ADR-0051, SPEC-06 §2

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-08.1 for traceability; the backlog story
> remains the authoritative work item. STORY-08.1 opens EPIC-08 (8/39).

## Summary
The SPEC-06 §2 hybrid retrieval query: one SQL round trip that ranks a tenant's live
chunks by vector similarity (pgvector hnsw) and by full-text relevance (gin), fuses
the two by reciprocal rank fusion (RRF, k=60), applies the source / uri-prefix /
date-range / metadata-tag filters, tunes `hnsw.ef_search` per query, and returns the
top-k ranked chunks with their score and citation metadata. Reached only through a
`*tenant.DB` from the resolver (ADR-0003, C-1, C-3). This story is the retrieval
query + benchmark only; the `/retrieve` endpoint (08.2), reranking (08.3) and the
answer path (08.4/08.5) are the seams that consume the returned `[]retrieve.Result`.

## Scope
- `internal/retrieve/retrieve.go`: `Retrieve(ctx, *tenant.DB, Params) ([]Result, error)`,
  the static hybrid SQL, filter-arg building, LIKE escaping, `ef_search` computation,
  the transaction-local GUC tuning, and `RRFScore` (the Go statement of the fusion
  formula, used as the test oracle). Plus `Explain`/`ExplainIndexed` for the benchmark.
- `internal/tenant/db.go`: `BeginRead` — a read-only transaction permitted on a
  suspended tenant (retrieval needs a tx to scope `set local hnsw.ef_search`).
- `test/e2e/retrieve_e2e_test.go`: golden-path correctness against a real enrolled
  tenant DB. `test/e2e/retrieve_bench_test.go`: the latency/plan benchmark harness.
- Not in scope: the HTTP endpoint (08.2), reranker (08.3), LLM/answer/citations/
  grounding (08.4/08.5), streaming (08.6), history (08.7), query logging (08.8).
  Query-embedding of the incoming question is the caller's job (08.2) — `Retrieve`
  consumes a query embedding, keeping it embedding-provider agnostic.

## Resolution
- **HNSW-friendly liveness (ADR-0051, the load-bearing change).** Ranking the vector
  CTE over the `live_chunks` view (as SPEC-06 §2 originally wrote) never uses the hnsw
  index — the view's `documents` join forces a full sort (≈180 ms at 50 k, seconds at
  1 M), so the spec's own SQL could not meet its own §7 budget. Fixed by ranking the
  `chunks` table directly with liveness as a semi-join on `version_id`
  (`version_id in (select current_version from documents where status='active' …)`),
  which is semantically identical to `live_chunks` (version_id is globally unique) but
  lets pgvector's iterative index scan drive the hnsw index. SPEC-06 §2 and §7 were
  updated in the same change; ADR-0051 records the decision + measured evidence.
- **GUC tuning.** `hnsw.ef_search = max(40, k_vector)` and
  `hnsw.iterative_scan = strict_order` are set transaction-locally via
  `set_config(name, $v, true)` inside a read-only tx, so they never leak to the pooled
  connection; `strict_order` keeps the RRF ranks in true distance order. Requires
  pgvector ≥ 0.8 (the pinned image); the SET fails loudly on an older extension.
- **Injection-safe filters.** All five filters compile into one static SQL string with
  fixed `$1..$10` binds; an absent filter binds NULL and is a `$n is null or …` no-op.
  The uri prefix binds a LIKE pattern with `%`/`_`/`\` escaped (`like $7 escape '\'`),
  so a client `%` is a literal. Empty source list binds NULL (not `= any('{}')`).

## Verification
- TDD: `internal/retrieve/retrieve_test.go` was written first and watched **red**
  (undefined symbols), then **green** — it pins the pure logic: `RRFScore` matches the
  spec formula and ranks a hybrid winner highest, `efSearch = max(40, k)`,
  `likePrefixPattern` escapes LIKE metacharacters, `withDefaults` applies ADR-0007
  values, and `buildArgs` lays out $1..$10 with absent filters as nil (incl. the empty
  source-list-is-nil trap). `internal/tenant/db_test.go` gained a red→green test that
  `BeginRead` does not refuse a read-only tenant.
- e2e correctness (`test/e2e/retrieve_e2e_test.go`, real enrolled tenant DB via a
  resolver + `*tenant.DB`, seeded through the real write store): fusion ranks the chunk
  that tops both lists first with the exact `RRFScore(1,1)` score; the source /
  uri-prefix / metadata-tag / date-range filters each narrow correctly; a client `%`
  in the uri prefix matches nothing (escaped); a soft-deleted document drops out
  (live-chunks semantics preserved).
- Benchmark (`test/e2e/retrieve_bench_test.go`): measured **p50 27.9 ms / p95
  32.8 ms / p99 41.5 ms** over 300 queries at **100 000 live chunks (dim 1536)**;
  EXPLAIN confirms the natural plan uses `chunks_embedding_idx` (hnsw) +
  `chunks_tsv_idx` (gin). A generic prepared-statement plan first made this ~648 ms
  p50 — fixed with a transaction-local `plan_cache_mode = force_custom_plan`
  (ADR-0051). The AC's p95 ≤ 120 ms at 1 M chunks is a documented target to confirm
  on production-class hardware (seeding 1 M is infeasible in dev; the harness runs at
  the largest local scale and never fabricates the 1 M number).
- No migration (schema + hnsw/gin indexes already exist); no OpenAPI change (no
  endpoint yet); schema-drift guard unaffected.
