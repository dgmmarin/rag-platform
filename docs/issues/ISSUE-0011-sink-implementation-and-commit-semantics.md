# ISSUE-0011: Sink implementation and commit semantics

**Type:** Feature · **Status:** Done · **Story:** STORY-05.6 · **Traces:** FR-ING-07, NFR-REL-02, SPEC-05 §1, SPEC-05 §5, SPEC-05 §8, ADR-0038

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.6 for traceability; the backlog
> story remains the authoritative work item.

## Summary
The ingestion **Sink** (SPEC-05 §1/§5): the per-document orchestrator that ties the
STORY-05.1–05.5 stages together so a connector (EPIC-06/07) can push documents at it.
`Sink.Put` runs parse → normalise/hash → hash short-circuit → chunk → embed → commit
(one transaction), and `Sink.Complete` soft-deletes documents not re-seen this run on
a full sync only. It accumulates the SPEC-05 §6 `Stats` the worker (STORY-05.7) will
persist.

## Scope
- New `internal/ingest/sink`, pure orchestration (owns no SQL): composes
  `parse.Registry`/`sidecar.Client` (routing `ErrUnsupportedMIME` to the sidecar),
  `chunk.Document`, `embed.Embedder`, and `documents.Store` — every tenant write
  through a `*tenant.DB` from the resolver (ADR-0003, C-3).
- Two new store methods in `internal/documents` (`TouchIfUnchanged`,
  `SoftDeleteUnseen`) on `TenantStore` + the `Store` interface.
- Not in scope: the connector framework (EPIC-06), the River worker binary
  (STORY-09.1), the `jobs.stats`/`usage_daily` writes and 100-error cap (STORY-05.7),
  reindex (05.8), GC (05.9).

## Resolution
- `sink.go`: `Sink`, `Config`, the connector-facing `Document`, `Stats`/`DocError`
  (JSON tags matching the SPEC-05 §6 `jobs.stats` shape), `Mode`
  (`Incremental`/`Full`), `SnoozeError`, and the injected `LocalParser`/
  `SidecarParser`/`Store` ports. `Put` implements the SPEC-05 §1 flow and stamps every
  chunk with the configured embedding model (Invariant 3) and its aligned vector;
  `Complete` soft-deletes unseen docs on a full sync only; `Stats()` snapshots with
  `duration_ms`.
- `documents/sync.go`: `TouchIfUnchanged` — the atomic SPEC-05 §1 hash short-circuit
  (one `update … from document_versions … where content_hash = $hash`) that touches
  `last_seen_at`, reactivates a byte-identical reappearance, and returns whether it
  matched, so an unchanged document costs no embedding; `SoftDeleteUnseen` — the
  full-sync `Complete` sweep (`last_seen_at < started_at → status='deleted'`).
- Error classification (ADR-0038): parse error or non-circuit embed error → recorded
  in `stats.errors`, `docs_failed++`, sync continues (§2/§8); `embed.ErrCircuitOpen`
  → `*SnoozeError` (worker snoozes, §8); store/DB error → propagated (fail the job).
  Crash safety is the store's per-document transaction (ADR-0008) plus hash-skip on
  retry.
- SPEC-05 §5 updated to record the realisation; ADR-0038 records the design.

## Verification
- TDD: `internal/ingest/sink/sink_test.go` written first and watched fail
  (undefined `New`/`Config`/`Document`) before implementation. `go test
  ./internal/ingest/sink` green, `-race` clean: changed → embed+store, unchanged →
  embed skipped, `ErrUnsupportedMIME` → sidecar, parse failure → `docs_failed` +
  continue, circuit-open → `*SnoozeError` wrapping `ErrCircuitOpen`, non-circuit embed
  error → recorded, store error → propagated (not a snooze), full `Complete` soft-
  deletes / incremental does not, stats + `bytes_fetched` default + duration.
- DB e2e (`test/e2e/sink_e2e_test.go`, build tag `e2e`) over a REAL enrolled tenant
  (`go test -tags e2e -run TestSinkFullSyncCommitSemantics ./test/e2e/...`, **PASS**,
  74.5 s): a full sync commits A/B/C with `current_version` visible in `live_chunks`;
  a mid-transaction failure (a wrong-dimension chunk) leaves NO partial document
  (rollback); crash-and-retry re-sees A unchanged (skipped by hash, not duplicated),
  changes B, and drops the unseen C via full-sync `Complete`; an incremental
  `Complete` deletes nothing. The embedding provider is stubbed (external service);
  the store/DB path is real.
- `gofmt -l` clean; `go vet` clean; pinned `golangci-lint v2.13.1` **0 issues** on
  `internal/ingest/sink` and `internal/documents` (incl. the `forbidigo` `Unsafe()`
  ban). `go test ./internal/ingest/... ./internal/documents/...` green.

## Notes / not in scope
- Ceiling (ponytail, ADR-0038): `stats.errors` uncapped (STORY-05.7 caps at 100);
  `started_at` (worker wall clock) vs `last_seen_at` (DB clock) assumes NTP-synced
  clocks (strict `<` biases to preserve); a non-circuit embed error is recorded
  per-doc and relies on the breaker to escalate a sustained outage to a snooze.
- No schema/migration/OpenAPI change: the two store methods are new SQL over the
  existing tables; `jobs.stats`/`usage_daily` are written by STORY-05.7, not here.
- Coverage: `internal/ingest/sink` sits under `internal/ingest`, which reports SKIP
  against the gate (matches the path exactly, has no direct Go files); `internal/
  documents` is not a gated package.
- Environment: `docker compose exec` is pathologically slow here (~60 s), so the e2e
  reads the tenant id over the direct control pool instead of the `docker compose
  exec` psql helper; best-effort `t.Cleanup` teardown may skip on that slowness.
