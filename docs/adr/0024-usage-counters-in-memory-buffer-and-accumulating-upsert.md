# ADR-0024: Usage counters — a sanctioned in-memory counter with a periodic accumulating upsert

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-ADM-06, SPEC-10 §6, SPEC-02 §2, SPEC-07 (`GET /v1/usage`), C-3, FR-ACC-03

## Context
STORY-03.7 makes control-plane usage accounting (SPEC-10 §6) a first-class
subsystem: the platform aggregates daily per-tenant counters — queries, documents
ingested, chunks embedded, embed tokens, and LLM in/out tokens (FR-ADM-06) — and
exposes them for billing/dashboards via `GET /v1/usage`. The `usage_daily` table
already exists (SPEC-02 §2) with a column per counter and a `(tenant_id, day)`
primary key, so no migration is needed and the drift guard stays green.

Like the audit log (ADR-0023), usage is a control-plane **write path that many
subsystems call into** as they land — the API increments `queries`, ingestion
increments `docs_ingested`/`chunks_embedded`/`embed_tokens`, retrieval increments
LLM tokens — plus a tenant-scoped **read surface**. This ADR records how the write
path is shaped and how the read is scoped, mirroring how STORY-03.6 deferred its
per-action write wiring while shipping the writer + reader now.

Sub-decisions this ADR records:
1. Whether counters are written synchronously per request or buffered and flushed.
2. How increments are merged into `usage_daily` (overwrite vs. accumulate).
3. How the read is scoped and where the tenant comes from.
4. What happens to an unscoped or failed increment.

## Decision
- **Producers increment an in-memory `usage.Counter`; a flush loop drains it every
  30 s (SPEC-10 §6), not one DB write per request.** `Counter.Add(tenantID, Delta)`
  is the sanctioned, non-blocking write entry point other subsystems adopt (the
  usage analogue of `audit.Record`): it merges a `Delta` into a per-`(tenant, day)`
  bucket under a mutex with no I/O, so it is safe on a request/job hot path.
  `Counter.Run(ctx, interval)` ticks every `DefaultFlushInterval` (30 s) and drains
  once more on shutdown so the last window is not lost. A synchronous upsert per
  query was rejected: it puts a control-plane round trip on every hot-path request
  (violating the ≤60 s-lag budget's intent of *cheap* accounting) and creates write
  contention on one `usage_daily` row per tenant/day. Buffering trades ≤30 s of lag
  (well within FR-ADM-06's tolerance) for a single coalesced upsert per bucket per
  flush.
- **Each bucket is written with an accumulating upsert** —
  `insert ... on conflict (tenant_id, day) do update set col = usage_daily.col +
  excluded.col` (SPEC-10 §6). Counters are *summed onto* the existing row, never
  overwritten, so repeated flushes — and multiple API/worker replicas each running
  their own counter — add correctly without a read-modify-write race. The upsert SQL
  is a pinned constant with a unit test asserting every column accumulates, because
  an accidental overwrite would silently lose counts.
- **A failed flush retains the counts.** `Flush` drains the buffer, and on the first
  upsert error it merges the un-flushed buckets back into the live buffer and
  returns the error; the next tick retries. `Run` treats a flush error as non-fatal
  and keeps counting through a transient control-plane blip. This makes the counter
  at-least-once within a process's lifetime rather than dropping usage on a hiccup.
- **The read is always tenant-scoped from the resolved credential (FR-ACC-03), not
  a parameter.** `Service.List` requires a tenant and fails closed on an empty one
  (matching `audit.Service.List`); `Handlers.List` serves `GET /v1/usage?from&to`
  taking the tenant from `tenant.TenantIDFromCtx` (401 if unresolved), exactly like
  the settings handlers (ADR-0022) — because `/v1/usage` is a tenant's own view of
  its usage (admin scope), not a cross-tenant platform-admin read like the audit
  endpoint. A range-less read defaults to the last 30 days so one request cannot
  scan unbounded history; an inverted range is a 400.
- **An unscoped or empty increment is dropped (fail closed), not written.**
  `Add` with an empty tenant, or a zero `Delta`, is silently discarded rather than
  writing to a blank/arbitrary tenant row — the same discipline as refusing an
  unscoped read. Counters are non-secret aggregates only (C-3); no tenant content
  ever reaches `usage_daily`.

## Consequences
- No migration and no schema change: `usage_daily` already exists, so the drift
  guard stays green.
- Counting is decoupled from the hot path: producers pay a mutex-guarded map merge,
  and the control plane sees at most one upsert per `(tenant, day)` per 30 s per
  replica. The accumulating upsert makes multiple replicas safe by construction.
- Usage is at-least-once per process lifetime, not exactly-once: a hard crash
  between a flush interval loses up to ~30 s of un-flushed counts. This is an
  accepted trade for FR-ADM-06 (approximate billing/usage aggregates), consistent
  with SPEC-10 §6 describing in-memory counters flushed periodically. Exactly-once
  would require a durable write per event, which sub-decision 1 rejected.
- Per-producer wiring lands with each producer (queries in EPIC-08 retrieval,
  docs/chunks/embed tokens in EPIC-05 ingestion, LLM tokens in EPIC-08 answering),
  each calling `Counter.Add` — mirroring how audit's per-action writes were deferred
  to their handlers (ADR-0023). STORY-03.7 delivers the counter, the flush loop, the
  reader, and the endpoint, with only mounting on the router (STORY-04.1) and the
  process-lifetime `Run` wiring in `ragctl serve`/`work` (EPIC-04/09) deferred.
- Router wiring is STORY-04.1, consistent with every other control-plane handler
  (ADR-0019/0021/0022/0023): `usage.Handlers.List` is built and tested (unit with
  fakes + `net/http/httptest`, and a real-Postgres e2e that flushes twice and reads
  the accumulated row back), with only mounting deferred.
