# ADR-0038: Ingestion sink and commit semantics — per-document orchestration composing the built stages, atomic hash short-circuit (`TouchIfUnchanged`), full-only `Complete` soft-delete, and snooze-vs-record error classes

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ING-07, NFR-REL-02, SPEC-05 §1, SPEC-05 §5, SPEC-05 §8 · **Decisions:** ADR-0008, ADR-0003, ADR-0033, ADR-0037

## Context
SPEC-05 §1 defines a per-document flow — parse → normalise → hash → compare to the
current version → (unchanged ⇒ touch `last_seen_at`) or (changed ⇒ chunk → embed →
commit) — and §5 the commit semantics (one transaction per document; the tx is not
opened until embeddings exist). STORY-05.1–05.5 built the stages: the Go/sidecar
parsers (ADR-0034/0035), the chunker (ADR-0036), the `Embedder` (ADR-0037), and the
document/version store `documents.TenantStore.Put` that already does the ADR-0008
single-transaction version-insert-and-`current_version`-flip (ADR-0033). STORY-05.6
ties them together as the ingestion **Sink** a connector (EPIC-06/07) pushes
documents at, and adds the full-sync `Complete` soft-delete. The AC: per-document
transaction; `Complete` marks unseen documents deleted on a full sync only; a worker
crash mid-sync leaves no partial document (NFR-REL-02).

## Options
- **Where the orchestration lives / owns no SQL.** The sink composes existing seams
  and holds no SQL of its own: every tenant write goes through `documents.Store` and
  thus a `*tenant.DB` from the resolver (ADR-0003, C-3). New package
  `internal/ingest/sink` (under `internal/ingest`, so it reports SKIP against the
  coverage gate exactly as ADR-0033/0037 note — the gate matches `internal/ingest`
  and it has no direct Go files). Rejected: putting orchestration in the worker
  (STORY-09.1) — the sink is pure composition and unit-testable without River.
- **Hash-before-embed short-circuit — a new store method vs reusing `Put`.**
  SPEC-05 §1 compares the content hash to the current version *before* chunk/embed so
  an unchanged document costs no embedding. `documents.Put` also compares the hash,
  but only *after* embeddings exist (it requires them at commit, ADR-0033/§5), so
  reusing it cannot save the embed. Options: (a) expose the current hash bytes and let
  the sink compare — rejected: two round trips (read then a separate touch) and the
  comparison logic leaks out of the store. (b) **add `TouchIfUnchanged(db, source,
  external, hash) (bool, error)`** (chosen): ONE `update … from document_versions …
  where v.content_hash = $hash` that touches `last_seen_at` and returns whether it
  matched — atomic, one statement, the compare stays in the store next to `Put`. It
  also reactivates a soft-deleted document that reappears byte-identical (`status =
  'active', deleted_at = null`), the no-embed analogue of the reactivation `Put` does
  on a changed reappearance. On no match (new or genuinely changed) the sink proceeds
  to chunk → embed → `Put`, which re-reads and re-compares for its own rollback/version
  logic (ADR-0033) — the one extra read on the changed path is the inherent price of
  hash-before-embed.
- **Full vs incremental `Complete`.** `Sink.Complete` soft-deletes documents of the
  source with `last_seen_at < run.started_at` **only on a full sync** — a full sync has
  re-listed the whole corpus, so absence means deletion; an incremental sync (sitemap
  `lastmod`, API cursor) has not, so `Complete` is a no-op. Realised as a second store
  method `SoftDeleteUnseen(db, source, startedAt) (int, error)` (one guarded `update …
  set status='deleted' where last_seen_at < startedAt and status <> 'deleted'`), gated
  by a `Mode` on the sink. `started_at` is captured at `New` from an injectable clock
  (default `time.Now`). **Cross-clock note:** `started_at` is the worker's wall clock
  while `last_seen_at` is the DB clock (Postgres `now()`); documents touched this run
  carry `last_seen_at > started_at` and are preserved, which assumes the worker and DB
  clocks are reasonably synced (NTP) — standard, and the sweep is strict `<` so a
  same-instant tie preserves rather than deletes.
- **Error classification — snooze vs record-and-continue vs fail.** SPEC-05 §2/§8
  wants a single-document parse error *recorded and skipped* (job succeeds with
  `docs_failed>0`), a provider-quota/circuit condition *snoozed* not failed, and no
  partial documents on a crash. The sink returns:
  - **nil (recorded, continue)** for a per-document terminal failure — a parse error
    (Go parse error, or sidecar `ErrParseFailed`/`ErrUnsupportedFormat`) *or a
    non-circuit embed error*. The document is added to `stats.errors` and
    `docs_failed++`; because the commit tx is never opened it keeps its previous
    version (SPEC-05 §5). A *sustained* provider outage is not silently swallowed: the
    `Embedder`'s per-provider breaker (ADR-0037) trips after N failures and the next
    call returns `ErrCircuitOpen`, escalating to a snooze.
  - **`*SnoozeError`** wrapping `embed.ErrCircuitOpen` — the worker snoozes the whole
    job (SPEC-05 §8, "provider quota exhausted → paused, not failed") rather than
    failing it; nothing was committed, so the retry re-processes from scratch.
  - **the underlying error (fail the job)** for an infrastructure failure
    (`TouchIfUnchanged`/`Put`/`SoftDeleteUnseen` DB errors). River retries the whole
    sync; completed documents stay whole (per-document tx) and unchanged ones are
    skipped by hash.
- **Crash safety proof.** The per-document transaction is `documents.Put`'s (ADR-0008):
  a crash between documents leaves each committed document whole and each unseen one
  absent; the retry skips the committed ones by hash (`TouchIfUnchanged`). A crash
  *inside* a commit rolls the whole version+chunks back — no partial document is ever
  visible on `live_chunks`.
- **Ports and testability.** The sink depends on small injected interfaces —
  `LocalParser` (`parse.Default()`), `SidecarParser` (`sidecar.Client`),
  `embed.Embedder`, and a `Store` (a 3-method subset of `documents.Store`, satisfied
  structurally by `documents.TenantStore`). The `*tenant.DB` is an opaque pass-through
  the sink holds and threads to the store (nil in unit tests, where a fake `Store`
  ignores it — a real handle is unforgeable by design, ADR-0003). This makes the
  orchestration hermetically unit-testable (fake store/embedder + the real Go parser
  registry + `httptest`-free fakes) and defers the real DB commit semantics to an e2e
  with a *stub* `Embedder` (the embedding provider is an external service; the sink is
  what is under test).
- **Stats.** The sink accumulates a `Stats` struct whose JSON tags match the SPEC-05
  §6 `jobs.stats` shape (`docs_seen/changed/unchanged/deleted/failed`,
  `chunks_written`, `embed_tokens`, `bytes_fetched`, `duration_ms`,
  `errors:[{external_id, msg}]`). This story fills the struct; the actual
  `jobs.stats`/`usage_daily` persistence and the 100-error cap are STORY-05.7, so
  `errors` is left uncapped here (ponytail, named ceiling + STORY-05.7 upgrade path).

## Decision
Add `internal/ingest/sink`: `Sink`, `Config`, `Document` (the connector-facing input),
`Stats`/`DocError`, `Mode` (`Incremental`/`Full`), `SnoozeError`, and the
`LocalParser`/`SidecarParser`/`Store` ports. `Sink.Put` runs the SPEC-05 §1 flow —
parse (Go registry, routing `parse.ErrUnsupportedMIME` to the sidecar), normalise +
`sha256`, `TouchIfUnchanged` short-circuit, chunk, embed, `Store.Put` in one
transaction — stamping every chunk with the configured embedding model (Invariant 3)
and its aligned vector, and folding token usage into `stats.embed_tokens`.
`Sink.Complete` soft-deletes unseen documents on a full sync only. Add two store
methods to `internal/documents` (`TouchIfUnchanged`, `SoftDeleteUnseen`) on
`TenantStore` and the `Store` interface, both reached only through a `*tenant.DB`
(ADR-0003). SPEC-05 §5 is updated to record the realisation.

## Consequences
- The worker (STORY-09.1) builds one `Sink` per sync job: resolve the tenant DB, pick
  the `Embedder` from `settings.providers_allowed` + model (ADR-0037), set the chunker
  config from settings, choose `Full`/`Incremental` from the schedule, drive
  `Put` per fetched document, call `Complete`, then persist `Stats` into `jobs.stats`
  and `usage_daily` (STORY-05.7 / ADR-0024) — inspecting `*SnoozeError` to snooze
  rather than fail (SPEC-08 §4/§5).
- A new connector or embedding provider needs no change here: the sink consumes the
  `Document` push and the injected `Embedder` (NFR-MNT-01/02).
- Crash safety is the store's per-document transaction plus the hash skip on retry;
  the sink adds no partial-write path of its own (it owns no SQL).
- **Ceiling (ponytail):** (1) `stats.errors` is uncapped — STORY-05.7 caps it at 100
  when it persists `jobs.stats`; (2) `started_at` vs `last_seen_at` is a cross-clock
  comparison assuming NTP-synced worker/DB clocks (strict `<` biases to preserve); (3)
  a non-circuit embed error is recorded per-document and relies on the breaker to
  escalate a sustained outage to a snooze — a provider that fails *without* tripping
  the breaker would mark documents failed rather than snooze (bounded by the breaker
  threshold). None affect isolation or leave partial data.
- No schema/migration/OpenAPI change: `TouchIfUnchanged`/`SoftDeleteUnseen` are new SQL
  over the existing `documents`/`document_versions` tables; `jobs.stats`/`usage_daily`
  are written by STORY-05.7, not here. `internal/ingest/sink` sits under
  `internal/ingest` (coverage-gate SKIP; the subpackage is not gated).
- Tests: hermetic unit tests (changed → embed+store, unchanged → embed skipped,
  `ErrUnsupportedMIME` → sidecar, parse failure → `docs_failed` and continue,
  circuit-open → `*SnoozeError`, non-circuit embed error → recorded, store error →
  propagated, full `Complete` soft-deletes / incremental does not, stats + duration
  accumulation) and a DB e2e over a real enrolled tenant (`test/e2e/sink_e2e_test.go`)
  proving the per-document transaction, the `live_chunks` flip, no partial document on
  a mid-transaction failure, crash-retry-by-hash, and full-vs-incremental `Complete`.
