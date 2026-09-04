# ADR-0051: Hybrid retrieval query — the `retrieve.Retrieve` seam, HNSW-friendly liveness filtering with iterative index scan, transaction-local `ef_search`, static filter binds, and the benchmark method

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-RET-01, FR-RET-02, FR-RET-08, NFR-PERF-01, SPEC-06 §2, SPEC-03 §2 (Invariants 1–2), C-1, C-3 · **Decisions:** ADR-0003, ADR-0004, ADR-0007, ADR-0008

## Context
STORY-08.1 delivers the hybrid retrieval query: one SQL round trip that ranks a
tenant's live chunks by vector similarity and by full-text relevance and fuses the
two with reciprocal rank fusion (RRF, k=60), with filters for source, uri prefix,
date range and metadata tags, and `hnsw.ef_search` tuned per query — meeting a p95
≤ 120 ms budget at 1 M chunks (SPEC-06 §2/§7, ADR-0007). It is the retrieval
half of EPIC-08; the query API that embeds the incoming question (STORY-08.2),
reranking (08.3), the grounding floor and the LLM answer path (08.5) are downstream
and consume the result slice this story returns. Query embedding is *not* produced
here — `Retrieve` takes the query vector plus the raw query text, keeping it
embedding-provider agnostic (the embed provider is ADR-0037).

Everything runs through a `*tenant.DB` from the resolver (ADR-0003, C-1, C-3); the
package holds no pool and no `Unsafe()`.

## Options and decisions

### Where the code lives and its shape
New package `internal/retrieve` with a single function
`Retrieve(ctx, *tenant.DB, Params) ([]Result, error)`. `Params` carries the query
embedding, the query text, the `k_vector`/`k_text`/`k` limits (ADR-0007 defaults
40/40/8) and a `Filters` value; `Result` is a ranked chunk with its RRF score and
the id/uri/title/heading_path/metadata a citation needs (never the embedding). This
signature is the seam 08.2/08.3 build on. Rejected: hanging retrieval off the
`documents` store — retrieval is a distinct read concern with its own SQL, and the
ISP precedent (ADR-0038 keeps reindex/gc off the `Store` interface) applies.

### The live_chunks view defeats the hnsw index — the load-bearing decision
SPEC-06 §2's authoritative SQL ranks the vector side with
`from live_chunks order by embedding <=> $1 limit $3`. Measured on the pinned
image (pgvector 0.8.2, pg16), this **never uses the hnsw index**: `live_chunks` is
`chunks join documents where status='active' and current_version=version_id`, and
because the planner must resolve that join before it knows which chunks are live,
it computes the join and then does a **full Sort** by distance — O(all live
chunks). At 50 k chunks the vector CTE alone measured ~183 ms; at 1 M it is
seconds. The spec's own SQL therefore cannot meet the spec's own 120 ms budget —
a contradiction between SPEC-06 §2 (the query) and §7 (the budget) that this story
had to reconcile. Per the source-of-truth rule the performance requirement (the
NFR) wins, so the query is corrected and the spec updated in the same change.

Options measured (50 k live chunks, dim 512, `ef_search=40`; EXPLAIN ANALYZE):

| Form | Uses hnsw | Exec |
|---|---|---|
| `from live_chunks order by <=> limit` (spec form) | no (Sort) | ~183 ms |
| … + `hnsw.iterative_scan=relaxed_order` | no (Sort) | ~113 ms |
| `from chunks where exists(documents live) order by <=> limit` + iterative | no (Sort) | ~107 ms |
| **`from chunks where version_id in (select current_version from documents where status='active') order by <=> limit` + iterative** | **yes** | **~6 ms** |
| bare `from chunks order by <=> limit` (no filter) | yes | ~0.8 ms |

**Chosen:** rank the vector (and, symmetrically, the full-text) CTE over the
`chunks` table directly, expressing liveness as a **semi-join on `version_id`**:
`c.version_id in (select d.current_version from documents d where d.status='active'
…)`. Because `version_id` is globally unique, a chunk's `version_id` is in that set
iff the chunk is the current version of its own active document — **semantically
identical to the `live_chunks` view** (SPEC-03 §2 Invariants 1–2), but expressed as
a chunk-column filter the planner can push into a pgvector *iterative index scan*.
The correlated `exists(...)` form does **not** work — the planner keeps sorting; only
the `version_id in (subquery)` form is recognised as an index-scan filter. The
optional uri-prefix and metadata-tag document filters fold into that same subquery,
so document-level filters narrow candidates without a second `documents` join in the
ordering path. The final SELECT joins `chunks`+`documents` on the ~k fused ids only,
to return uri/title.

### Iterative index scan (pgvector ≥ 0.8)
Filtered `order by <=> limit` needs `hnsw.iterative_scan` so the index scan resumes
until it has `k_vector` rows that pass the filter. It is set to **`strict_order`**
(not `relaxed_order`) so the returned rows are in exact nearest-neighbour distance
order — the `row_number()` ranks the RRF fusion consumes are then the true ranks.
This requires pgvector ≥ 0.8 (the platform's pinned image). On an older extension
the `SET` errors and retrieval fails loudly, rather than silently degrading to
full-sort latency; the requirement is documented in SPEC-06 §2.

### The generic-plan trap — `plan_cache_mode = force_custom_plan`
With the query correct and hnsw-eligible, the benchmark still measured a per-call
p50 of ~180 ms at 100 k while a one-off `EXPLAIN ANALYZE` of the identical query
measured ~15 ms server-side — a 12× gap independent of `iterative_scan` mode. The
cause: the optional filters are `$n is null or <predicate>` guards, and pgx caches
prepared statements, so after a few executions PostgreSQL switches the statement to
a **generic plan**. A generic plan cannot see that (say) `$2` is NULL, so it cannot
prune the guard and falls back to a scan-and-sort plan instead of the hnsw index.
An `EXPLAIN` is never a cached prepared statement, so it is always custom-planned —
which is why the one-off EXPLAIN looked fine while real traffic did not.

**Fix (measured):** set `plan_cache_mode = force_custom_plan` transaction-locally.
It re-plans per execution with the actual parameter values, the planner prunes the
NULL guards and selects the hnsw + gin indexes, and p50/p95 dropped to ~13/15 ms at
100 k (from ~180 ms). This is the standard remedy for parameterised queries with
optional filters over an index; it is scoped to the retrieval transaction, so the
tenant pool's other prepared statements are unaffected.

### `hnsw.ef_search` and the transaction
`ef_search` is set per query to `max(40, k_vector)` (SPEC-06 §2). GUCs cannot be
bound parameters, so all three (`hnsw.ef_search`, `hnsw.iterative_scan`,
`plan_cache_mode`) are set with `select set_config(name, $value, true)` —
`is_local=true` scopes them to the current transaction, `set_config`'s value
argument is a normal bind (no string interpolation into SQL), and the `ef_search`
value is a validated int. The query runs inside a **read-only transaction** opened
via a new `tenant.DB.BeginRead` — unlike `Begin` it does not refuse a suspended
(read-only) tenant, because a read is always safe and it opens the tx with
`pgx.ReadOnly` access mode so no write can slip through; retrieval on a suspended
tenant stays available, consistent with the other read paths that use `db.Query`.
The transaction-local sets never leak back to the pooled connection.

### Filters — static SQL, no fragment concatenation
All five filters are compiled into a single **static** SQL string with fixed binds
`$1..$10`; an absent filter is passed as a NULL bind and guarded by
`$n is null or …`, exactly the spec's `$2::uuid[] is null or …` pattern. Nothing is
concatenated from caller input, so there is no injection surface. The uri-prefix
filter binds a LIKE pattern whose `%`, `_` and `\` are escaped in Go (`like $7
escape '\'`), so a client-supplied `%` is a literal, never a wildcard. An empty
source list binds NULL (not an empty array, which `= any('{}')` would turn into
"match nothing"). Date range filters `chunks.created_at` (the indexing timestamp,
the only timestamp `live_chunks`/`chunks` exposes); metadata tags use `documents.
metadata @> $10`, backed by its gin `jsonb_path_ops` index.

### Benchmark method and the 1 M honesty caveat
`test/e2e/retrieve_bench_test.go` (build tag `e2e`) seeds a realistically-shaped
corpus (many documents, ~10 chunks each, per-row deterministic pseudo-random
vectors, full-text terms), runs N warmed queries through `Retrieve`, and reports
p50/p95/p99. It also EXPLAINs the query and asserts index-eligibility. Corpus size
and dimension are env-tunable (`RETRIEVE_BENCH_CHUNKS`, `RETRIEVE_BENCH_DIM`, …) so
it *can* run at 1 M against production-class Postgres. Seeding 1 M chunks (HNSW
insert maintenance is the bottleneck) is not feasible in this dev environment, so
the harness runs at the largest local scale and **the 1 M p95 is a documented
target to confirm on real hardware — it is never fabricated** (mirroring the
extraction-corpus human-review caveat). The p95 ≤ 120 ms assertion fires only when
the run actually seeded ≥ 1 M chunks; the plan-uses-both-indexes assertion fires at
≥ 10 k.

**Measured here** (300 queries, 100 000 live chunks across 10 000 documents, dim
1536, `ef_search=40`, warm cache; single local pgvector 0.8.2 container):
**p50 = 27.9 ms, p95 = 32.8 ms, p99 = 41.5 ms** (min 11.5, max 47.6). The natural
EXPLAIN plan uses `chunks_embedding_idx` (hnsw) and `chunks_tsv_idx` (gin). Before
the `force_custom_plan` fix the same run measured p50 = 648 ms / p95 = 811 ms — the
generic-plan regression above; the isolated fix probe measured p50 181 ms → 13 ms.
Extrapolation: HNSW query cost grows ~log(N); 100 k at p95 32.8 ms leaves ~3.6×
headroom under the 120 ms budget, so 1 M is expected to hold comfortably — **to be
confirmed on production-class hardware** (seeding 1 M chunks — HNSW insert
maintenance — is infeasible in this dev container; the benchmark asserts the budget
automatically when run with `RETRIEVE_BENCH_CHUNKS=1000000`).

## Consequences
- Retrieval is index-time at scale: both the hnsw and gin indexes back the one-round-
  trip fused query. The correction is transparent to callers — same semantics as
  `live_chunks`, same RRF fusion, same result shape.
- SPEC-06 §2 is updated to the corrected SQL and records the iterative-scan / pgvector
  ≥ 0.8 requirement (NFR-MNT-04: the spec and code move together).
- A dependency was added on pgvector's iterative index scan (≥ 0.8), already the
  platform's pinned image; a downgrade breaks retrieval loudly, by design.
- `tenant.DB.BeginRead` is new surface; it touches `internal/tenant`, so the isolation
  suite (SPEC-01 §9) is re-run. It cannot widen access — it is read-only and still the
  only path to a tenant is the resolver-issued handle.
- No migration: the `chunks`/`live_chunks`/`documents` schema and its hnsw+gin indexes
  already exist. No OpenAPI change: no endpoint yet (STORY-08.2).
