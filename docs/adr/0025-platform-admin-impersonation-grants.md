# ADR-0025: Platform admin impersonation — a scoped, audited, time-bounded grant that never swaps identity silently

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-ACC-07, SPEC-02 §4/§6, SPEC-09 §3, C-3, FR-ACC-03

## Context
STORY-03.8 lets a platform admin (`users.is_platform_admin`) impersonate a tenant
user for support (FR-ACC-07): assume that user's identity/tenant scope to act on
their behalf. SPEC-02 §4 requires every such action to be audited with
`details.impersonation=true`, and SPEC-02 §6 lists `admin.impersonate` in the
minimum audit set. The tenant-less `RequirePlatformAdmin` middleware (ADR-0023)
already gates cross-tenant platform-admin endpoints and was introduced anticipating
exactly this story; the sanctioned `audit.Record` writer (ADR-0023) is the audit
path.

Open questions this ADR settles:
1. How the impersonated identity is represented — a silent session swap vs. an
   explicit, attributable grant.
2. Where the grant lives and whether it needs a schema change.
3. How the grant is bounded, revoked, and audited.
4. How the impersonation service reaches the sanctioned audit writer without
   coupling to it.

## Decision
- **An explicit `impersonation_sessions` grant, not a silent identity swap.** Each
  grant records BOTH the real admin actor (`admin_user_id`) AND the impersonated
  principal (`tenant_id` + `impersonated_user_id`). Nothing overwrites the admin's
  own identity: the grant is a *record that admin A is acting as tenant T's user U*,
  so every downstream action stays attributable to A. This is the whole point of
  FR-ACC-07's "audit-logged" clause — a swapped session that presented U with no
  trace back to A would defeat it. A new control migration (`00006`) adds the table,
  mirrored into `schemas/control_plane.sql` so the drift guard stays green.
- **Control-plane only (C-3).** The grant references control-plane ids and touches
  no tenant data; `tenant_id`/`impersonated_user_id` are informational copies of
  control-plane ids (like every cross-boundary id, SPEC-03 §2 invariant 4). No
  tenant database is opened to start or end an impersonation.
- **Time-bounded and revocable, failing closed.** `expires_at` bounds the grant (a
  1 h default) so a forgotten session self-expires; `ended_at` is stamped on End so
  it is explicitly revocable. `Impersonation.Active(now)` is the single predicate:
  an ended or expired grant is inactive. Missing arguments and unknown ids are
  refused (`ErrNoImpersonation` → 404), matching the fail-closed posture of the rest
  of the auth package.
- **Only platform admins may start it; the actor comes from the session.** The
  Start/End handlers assume `RequireSession` + `RequirePlatformAdmin` upstream
  (reused, not reinvented) and read the acting admin from the session, never from a
  body field (FR-ACC-03) — a request cannot forge the actor. The e2e proves a
  non-admin is refused 403 by the real middleware and no grant/audit row is written.
- **Two audit events, written through the sanctioned writer via an injected seam.**
  Start writes `admin.impersonate`; End writes `admin.impersonate.end`
  (`admin.impersonate.end` is an append-only companion to the SPEC-02 §6
  `admin.impersonate` action, not a renumbering). Both carry actor = the real admin,
  target = the impersonated user, tenant = the impersonated tenant, and
  `details.impersonation=true` (SPEC-02 §4) plus non-secret ids only (C-3). The
  service holds an `AuditFunc` seam wired to `audit.Record` over the same pool on
  the real path; unit tests capture events through it. This avoids importing the
  audit writer's unexported command-tag interface into the service's own DB seam
  while still using the one sanctioned writer.

## Consequences
- The audit trail is complete for impersonation from day one: unlike the deferred
  per-action wiring for not-yet-built features (ADR-0023), impersonation is entirely
  new here, so its two events are wired in the same change that introduces it.
- Router wiring (mounting Start/End behind `RequireSession` + `RequirePlatformAdmin`)
  is STORY-04.1, consistent with every other control-plane handler (ADR-0019/0021/
  0022/0023): the handlers and service are built and tested (unit with fakes +
  `httptest`, and a real-Postgres e2e), with only mounting deferred.
- Enforcing impersonation *scope on downstream requests* (a request presenting a
  live grant is treated as the impersonated user, with the UI banner of SPEC-02 §4)
  layers onto this record when the public request pipeline exists (STORY-04.1): the
  grant is the durable, audited source of truth those layers consult. This story
  delivers the grant + audit primitive; the request-time application of it rides the
  same `RequirePlatformAdmin` gate.
- `impersonation_sessions` rows are retained as an audit-adjacent history (they are
  never the live-session store — sessions remain in the `sessions` table); a GC of
  long-expired grants can be added with the daily maintenance job (EPIC-09) if
  volume warrants, but is not needed for correctness.
