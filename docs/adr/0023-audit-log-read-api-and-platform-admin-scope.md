# ADR-0023: Audit log — a sanctioned append-only writer, a tenant-scoped read API, and platform-admin scope

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-ADM-05, FR-ACC-07, SPEC-02 §6, C-3

## Context
STORY-03.6 makes the control-plane audit log (SPEC-02 §6) a first-class subsystem:
every listed privileged action records a row, and platform admins can read a
tenant's history via `GET /admin/audit?tenant=`. The `audit_log` table already
exists (SPEC-02 §2) and several packages already append to it with direct inserts
— `internal/provision` (tenant.create/suspend/resume/move/delete-*) and
`internal/cp/tenants` (settings.update, ADR-0022). What was missing is (1) a single
sanctioned writer for actions still to be built, (2) a reader, and (3) an
authorization model for reading, since a platform admin reads *across* tenants and
so cannot use the tenant-resolved `RequireRole` middleware (ADR none; SPEC-02 §4).

Sub-decisions this ADR records:
1. Whether to add a shared writer now or keep per-caller inserts.
2. How the audit read is scoped and authorized.
3. Where the platform-admin-only authorization check lives.

## Decision
- **A sanctioned `audit.Record` writer is introduced now; existing writers converge
  onto it as they are next touched.** `internal/cp/audit` exposes
  `Record(ctx, db, Event)` — a single `insert into audit_log` that validates a
  non-empty action, defaults nil details to `{}`, and carries actor (user or API
  key), tenant, and target. It is the write half of the named subsystem, not
  speculative flexibility: SPEC-02 §6 enumerates the events it will record. The
  action handlers built in STORY-04.1 (member.\*, apikey.\*) call it, and the
  existing direct inserts in `provision`/`tenants` are left in place for now —
  converging them is a mechanical follow-up (ADR-0022 already anticipates the
  settings writer being "wrapped later") and was deliberately deferred to avoid
  churning just-shipped code and its test fakes. Details carry non-secret metadata
  only (C-3) — never credentials or setting values.
- **Reads are always tenant-scoped and fail closed.** `Service.List` requires a
  tenant and rejects an empty one, so no caller can fetch the whole log unscoped.
  Results are newest-first by `id`, page size defaults to 50 and is capped at 200,
  and `before=<id>` gives keyset pagination. The endpoint takes the tenant as a
  **query parameter**, not the resolved-credential tenant, precisely because a
  platform admin reads any tenant (FR-ACC-07) — the credential's own tenant is
  irrelevant here.
- **Authorization is a distinct, tenant-less `RequirePlatformAdmin` middleware.**
  Because the read is cross-tenant, `RequireRole` (which keys off the resolved
  tenant, ADR none/SPEC-02 §4) does not fit. `AuthzService.RequirePlatformAdmin`
  reads the session user and checks `users.is_platform_admin` in one query: 401
  with no session, 403 for a non-admin (including an unknown user id), else the
  handler runs. The handler itself does not re-check authorization — it assumes the
  middleware, mirroring how the settings handlers assume `RequireRole` (ADR-0022).
  This middleware is also the natural gate for STORY-03.8 (impersonation) and the
  platform-admin UI (STORY-11.7).

## Consequences
- No migration and no schema change: `audit_log` already exists, so the drift guard
  stays green. The reader reads every row regardless of which writer produced it,
  so the endpoint already returns tenant.\* and settings.update history today.
- Router wiring is STORY-04.1, consistent with every other control-plane handler
  (ADR-0019/0021/0022): the `audit.Handlers.List` handler and the
  `RequirePlatformAdmin` middleware are built and tested (unit with fakes +
  `net/http/httptest`, and a real-Postgres e2e that drives a genuine
  settings.update and reads it back), with only mounting deferred.
- A second authorization primitive now exists alongside `RequireRole`:
  tenant-scoped role checks vs. cross-tenant platform-admin checks. Keeping them
  separate avoids overloading `RequireRole` with a tenant-optional mode.
- Because per-action write wiring for not-yet-built features (source.\*, job.cancel,
  admin.impersonate) lands with those features, "every listed action writes a row"
  becomes fully true incrementally; the sanctioned writer means each does so the
  same way rather than reinventing the insert.
