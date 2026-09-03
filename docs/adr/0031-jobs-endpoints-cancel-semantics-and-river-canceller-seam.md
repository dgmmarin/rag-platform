# ADR-0031: Jobs endpoints — queued-cancel-now, running-cancel as a River seam

**Status:** Accepted · **Date:** 2026-09-03 · **Requirements:** FR-ADM-02, FR-ACC-03, C-3, SPEC-07 §1/§2, SPEC-08 §3/§4 · **Decisions:** ADR-0003, ADR-0005, ADR-0027, ADR-0028, ADR-0029

## Context
STORY-04.5 must deliver the SPEC-07 §2 jobs surface: `GET /v1/jobs`,
`GET /v1/jobs/{id}` and `POST /v1/jobs/{id}/cancel`. It traces FR-ADM-02 ("list
jobs with status, duration and statistics, and allow cancelling a queued job").

Two facts about the existing design shape the work:

- `jobs` is a **control-plane** table (`schemas/control_plane.sql`; C-3). It is
  the *history/mirror* view of the queue, not the queue itself (ADR-0005). The
  rows STORY-04.3/04.4 already write (`sync_source`, `delete_source`,
  `ingest_document`) live here. So the endpoints operate on the control-plane
  pool, tenant-scoped by the resolved credential (FR-ACC-03) — there is no
  `tenant.DB` on this path (ADR-0003), mirroring `internal/cp/sources`
  (ADR-0029), not the tenant-content documents path (ADR-0030).
- **There is no job worker yet.** River integration is EPIC-09 (STORY-09.1). So
  nothing sets a job to `running`, observes a cancel signal, or writes terminal
  transitions today.

The cancel semantics are defined by SPEC-08 §4: cancel "sets a flag"; River
cancels **queued** jobs immediately; **running** jobs observe `ctx.Done()`
*between documents* and exit with status `cancelled`, committing nothing partial.
SPEC-08 §3 assigns the mirror-row transitions (queued→running→terminal) to the
**worker middleware**, and the admin UI reads only `jobs`.

The tension to resolve: SRS FR-ADM-02 requires cancelling a **queued** job;
SPEC-07 §2 widens this to "queued or running". This is not a contradiction — the
SRS is the minimum and the SPEC realises a superset — so the API accepts both,
but only the parts that can be honoured *correctly today* are wired; the rest is
a fail-closed seam. The house rule (STORY-04.1/04.3/04.4 precedent) is: build the
HTTP layer and the real store for what the schema supports now, and inject the
not-yet-built pieces as seams that fail closed — do not build EPIC-09 work under
an EPIC-04 story.

## Options
- **Where jobs live / which pool.** (a) reach a tenant DB — rejected: jobs are
  control-plane rows (C-3), no tenant content is involved; (b) the control-plane
  `jobs` table, tenant-scoped by the resolved credential (chosen; same shape as
  ADR-0029 sources).
- **Cancelling a QUEUED job.** (a) route it through River — impossible, no River
  yet; (b) flip the mirror row `queued`→`cancelled` in one guarded SQL statement
  (`... where status='queued'`), stamping `finished_at` (chosen). A queued mirror
  row today has no worker holding it, so the mirror is authoritative and this is
  *fully effective now* — it satisfies FR-ADM-02's "cancel a queued job"
  literally. When River lands, enqueue+cancel move behind River and the same row
  transition is written transactionally; the guarded `where status='queued'`
  makes it race-safe either way.
- **Cancelling a RUNNING job.** (a) flip the mirror `running`→`cancelled` in the
  API — **rejected**: SPEC-08 §3 assigns running/terminal transitions to the
  worker, and SPEC-08 §4 says a running cancel is *cooperative* ("nothing
  partial"). With a worker present, the API flipping the row would race the
  worker (which would still be mid-flight and then write `succeeded`), and would
  falsely assert the job stopped — a fake (AGENTS.md Integrity). (b) a fake/no-op
  "cancelled it" response — rejected for the same reason. (c) a `Canceller` seam
  (the River `JobCancel` operation), nil until EPIC-09; a running cancel with the
  seam nil returns the **not_found seam envelope** (chosen), exactly mirroring
  STORY-04.3 `/test` (connector nil) and STORY-04.4 upload (storage nil). The
  cancel *signal* belongs in River (the queue), not the mirror, so no mirror
  column is added for it.
- **Cancelling a TERMINAL job.** (a) 404 — misleading (the job exists); (b) 409
  `conflict` for succeeded/failed, and an idempotent no-op for an
  already-`cancelled` job (chosen; matches SPEC-07 §1 codes).
- **Duration / statistics (FR-ADM-02).** (a) a new stats endpoint — over-scoped;
  (b) expose the existing `jobs` columns (`status`, `attempt`, `stats`, timing)
  and compute `duration_ms` from `finished_at − started_at` for finished jobs
  (chosen; a running job leaves it null and the client derives elapsed from
  `started_at`).
- **Audit of the cancel action (FR-ADM-05).** Deferred, consistent with
  STORY-03.6's plan ("job.cancel in EPIC-09") and with STORY-04.3/04.4 deferring
  their `source.*`/document audit wiring. The sanctioned `audit.Record` writer
  (STORY-03.6) is adopted when the worker/cancel path is finalised in EPIC-09.

## Decision
Add `internal/cp/jobs`: a `Store` (`PoolDB` over the control-plane pool) with
`List`/`Get`/`CancelQueued`, a `Service` (filter validation, keyset pagination,
duration computation, and the cancel state machine) holding an optional
`Canceller` seam, and `Handlers` speaking the SPEC-07 §1 envelope with the tenant
taken only from the resolved context (FR-ACC-03). Three routes are appended to
`New` (router.go) and to `liveRoutes()` (openapi.go) so the served spec, the
drift guard, and the contract tests (ADR-0028) grow with them; `api/openapi.yaml`
is regenerated by `mise run openapi`.

Cancel is a state machine over the current row:
- `queued` → guarded flip to `cancelled` (**effective now**), HTTP 200;
- `running` → `Canceller.Cancel` when wired (HTTP 202, cancellation requested,
  the worker finalises the row per SPEC-08 §3); nil today → `not_found` seam;
- `succeeded`/`failed` → 409 `conflict`; already `cancelled` → idempotent 200.

List supports `?status`, `?kind`, `?source` filters (validated against the enums)
and `?limit&cursor` keyset pagination (`{items, next_cursor}`).

## Consequences
- The jobs list/get surface and **queued-job cancellation** are live and
  e2e-tested against the real control-plane `jobs` table. FR-ADM-02's cancel
  requirement is met in full for the case that exists today.
- Running-job cancellation is a documented, fail-closed seam. EPIC-09
  (STORY-09.1/09.4) wires the `Canceller` (River `JobCancel`) and the worker
  middleware that honours `ctx.Done()` between documents and writes the
  `running`→`cancelled` transition (SPEC-08 §3/§4); no change outside
  `internal/cp/jobs` + its wiring is needed to activate it.
- No schema/migration change (the `jobs` table, its enums, and the
  `(tenant_id, queued_at desc)` index already exist), so the drift guard stays
  green.
- No tenant content is touched (C-3); the tenant is never a request parameter
  (FR-ACC-03); no `tenant_id` is added to any tenant table (C-1).
- The cancel action is not yet audited; that wiring rides EPIC-09 with the rest
  of the job lifecycle (FR-ADM-05), consistent with the STORY-03.6 plan.
