# Delivery backlog — epics & stories status

Expanded from `backlog_import.csv`. Epic and story readiness only; the task-level
breakdown lives in [`BACKLOG_TASKS.md`](BACKLOG_TASKS.md). Full narrative in
[`BACKLOG.md`](BACKLOG.md).

**Legend**
- ✅ **Done** — implemented, unit + e2e tests green, lint clean. (EPIC-01 work is currently uncommitted, pending review.)
- 🚧 **In progress** — some stories delivered, epic not yet complete.
- 🔲 **Todo** — not started.

## Summary by epic

| Epic | Title | Points | Done pts | Status |
|---|---|--:|--:|---|
| EPIC-01 | Project foundation | 21 | 21 | ✅ Complete |
| EPIC-02 | Tenancy core | 34 | 34 | ✅ Complete |
| EPIC-03 | Control plane services | 34 | 34 | ✅ Complete |
| EPIC-04 | Public API surface | 21 | 13 | 🚧 In progress |
| EPIC-05 | Ingestion pipeline | 42 | 0 | 🔲 Todo |
| EPIC-06 | Connector framework and upload connector | 13 | 0 | 🔲 Todo |
| EPIC-07 | Web crawl, sitemap and API connectors | 39 | 0 | 🔲 Todo |
| EPIC-08 | Retrieval and answering | 39 | 0 | 🔲 Todo |
| EPIC-09 | Jobs, scheduling and maintenance | 21 | 0 | 🔲 Todo |
| EPIC-10 | Security, observability, operations | 26 | 0 | 🔲 Todo |
| EPIC-11 | Admin UI (reference) | 34 | 0 | 🔲 Todo |
| EPIC-12 | Evaluation harness and quality | 13 | 0 | 🔲 Todo |
| **Total** | | **337** | **102** | **30%** |

---

## EPIC-01 · Project foundation — ✅ 21/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-01.1 | Repository skeleton and build | 3 | ✅ Done | ADR-0002 |
| STORY-01.2 | Local development stack | 3 | ✅ Done | — |
| STORY-01.3 | CI pipeline | 5 | ✅ Done | NFR-MNT-03 |
| STORY-01.4 | Configuration and secrets loading | 3 | ✅ Done | SPEC-09 §2 |
| STORY-01.5 | Control-plane migrations tooling | 2 | ✅ Done | SPEC-02 |
| STORY-01.6 | Logging, metrics, tracing scaffolding | 5 | ✅ Done | FR-OBS-01/02/03, SPEC-10 |

**Delivered:** `internal/{cli,config,crypto,migrate,obs}`, `cmd/ragctl`; Docker/compose local
stack + seed; goose control-plane migration with drift guard; AES-256-GCM envelope crypto with
age/AWS KMS; slog/metrics/tracing scaffolding and `serve` HTTP skeleton; GitHub Actions CI driving
mise tasks with a config-driven 70% coverage gate. ADRs 0010–0014.

## EPIC-02 · Tenancy core — ✅ 34/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-02.1 | Tenant registry and resolver | 8 | ✅ Done | FR-TEN-03, FR-ACC-03, SPEC-01 §2–4, ADR-0003 |
| STORY-02.2 | Tenant schema migrations | 5 | ✅ Done | FR-TEN-09, SPEC-01 §7, ADR-0015 |
| STORY-02.3 | Tenant provisioning job | 8 | ✅ Done | FR-TEN-01/02, SPEC-01 §6, ADR-0016 |
| STORY-02.4 | Tenant suspension, deletion and grace period | 5 | ✅ Done | FR-TEN-04/05, SPEC-01 §8, ADR-0017 |
| STORY-02.5 | Tenant move (connection update) | 3 | ✅ Done | FR-TEN-07 |
| STORY-02.6 | Isolation test suite | 5 | ✅ Done | NFR-SEC-01, SPEC-01 §9, ADR-0018 |

**Delivered (STORY-02.1):** `internal/tenant` — `DB` handle (the only tenant-SQL entry point, ADR-0003),
`Resolver.Open` applying the SPEC-01 §3 status rules (rw active / ro suspended / errors otherwise) with
fail-closed schema-version check, a 30s TTL-cached registry, and a lazy LRU pool cache with idle eviction
(SPEC-01 §4). Cache invalidation is driven by a `tenant_changed` LISTEN/NOTIFY loop backed by a new control
migration (`00002`) so suspension/move take effect within ~1s. Tenant identity in context is
observability-only. Unit tests + e2e golden path over the real Postgres (per-tenant role/database,
rw/ro/error resolution, and NOTIFY-driven invalidation). Coverage gate on `internal/tenant` met.

**Delivered (STORY-02.2):** `ragctl migrate tenants` — a parallel per-tenant goose runner in
`internal/migrate` (`tenant.go`, `tenant/00001_initial_schema.sql`) sharing the STORY-01.5 goose plumbing
and drift guard. One goose `Provider` per tenant (safe for the parallel fan-out); each tenant handled in a
single control-plane transaction that locks its `tenant_databases` row `FOR UPDATE SKIP LOCKED`, applies
pending migrations with the per-tenant role, and mirrors the version into `schema_version`. The `vector(N)`
dimension is a placeholder substituted per tenant from `settings.embedding_dim` via an in-memory FS; a
non-positive dimension fails closed. Failures are recorded and non-fatal — the command exits non-zero
listing failed slugs and a rerun resumes only those behind. The resolver's placeholder
`expectedSchemaVersion` is now derived from the embedded migrations (`migrate.ExpectedTenantVersion()`), so
`Open` fail-closed tracks what the runner applies. Extensions moved to provisioning (superuser) per SPEC-01
§6, since tenant migrations run as the least-privilege role. Unit tests (drift guard, placeholder
substitution, version derivation, input validation) + e2e golden path over the real Postgres including a
deliberately-failing tenant and a resuming rerun. ADR-0015.

**Delivered (STORY-02.3):** `internal/provision` — an idempotent tenant provisioner plus `ragctl enroll`.
A privileged (superuser) connection (`PROVISION_DB_URL`, falling back to the control-plane URL) creates the
least-privilege per-tenant role (`NOSUPERUSER NOCREATEDB NOCREATEROLE`) and its owned database, then installs
the three required extensions (`vector`, `pgcrypto`, `pg_trgm`) inside the new database — the superuser-only
step SPEC-01 §6/ADR-0015 assign to provisioning. The generated password is envelope-encrypted with the
platform `crypto.Cipher` (same DEK the resolver decrypts with, SPEC-09 §2) and recorded in `tenant_databases`;
migrations are applied via the STORY-02.2 runner (as the per-tenant role); the tenant is set active and a
`tenant.create`/`tenant.provision` audit event written in one control-plane transaction (never logging the
password). DDL identifiers are validated against a strict lowercase pattern and rejected if unsafe
(injection-proof). Re-running is idempotent: existing row/role/database are reused (the password is not
regenerated), so a retry — and the future River `provision_tenant` job (ADR-0005) — is safe. The async enqueue
is deferred to EPIC-09; enroll runs the same handler synchronously until then. Unit tests (identifier quoting,
password generation, SQL builders, validation/defaults, URL rewrite) + e2e golden path over the real Postgres
asserting role/db/extensions exist, migrations applied at the configured embedding dimension, status active,
password round-trip (decrypt + login as the role), and idempotent re-run. ADR-0016.

**Delivered (STORY-02.5):** `Lifecycle.Move` (`internal/provision/move.go`) plus `ragctl tenant move` — a
tenant move (connection update, FR-TEN-07, SPEC-01 §4). Move updates any subset of the
`tenant_databases` connection record (host/port/database/username/ssl_mode, and optionally a rotated
password) in one control-plane transaction that locks the tenant row and writes a `tenant.move` audit event
(non-secret metadata + a `password_rotated` flag only, C-3). A supplied password is envelope-encrypted with the
same platform Cipher the resolver decrypts with (SPEC-09 §2), so encrypt-on-write and decrypt-on-read stay
symmetric; an all-empty move is rejected (fail closed). The write fires `tenant_changed`, so the resolver
evicts the pool and invalidates its cached record within ~1s and the next `Open` rebuilds against the new
connection — no new eviction code was needed: `Resolver.Close` from STORY-02.1 already fully evicts the pool
and invalidates the registry cache. The `PATCH /admin/tenants/{id}` HTTP route (FR-TEN-07) is deferred to
EPIC-04 (STORY-04.6) since the public router does not exist until STORY-04.1, mirroring the enroll/suspend/
delete deferrals (ADR-0016/0017); `ragctl tenant move` is the sole entry point until then. Unit tests
(validation: no privileged URL, blank slug, empty params, password-without-encrypter, negative port) + e2e
golden path over the real Postgres asserting the resolver connects to the original database before the move,
to the new database after, the rotated password round-trips (decrypt back to plaintext) and the new role logs
in, and a `tenant.move` audit event is written. `docs/runbooks/move-tenant.md` documents the operator copy +
repoint procedure. No ADR: reuses the STORY-02.4 lifecycle pattern and the STORY-02.1 resolver eviction path.

**Delivered (STORY-02.6):** the isolation test suite (`test/e2e/isolation_e2e_test.go`) enrols two tenants
(A and B) end to end via `ragctl enroll` on the real stack and proves zero cross-tenant leakage at the layer
that exists today — the resolver + `tenant.DB` (the only path to tenant data, ADR-0003), since the public HTTP
router is STORY-04.1 (EPIC-04): resolving A's ID yields A's data and only A's (and B's ID yields B's — identity
from the registry, never a client parameter, FR-ACC-03); A's resolved connection cannot read B's rows; and A's
credentials against B's database are rejected by Postgres (with a control proving A still reaches its own DB).
Writing the suite surfaced a real gap — Postgres grants `CONNECT` to `PUBLIC` by default, so A's role could open
a session against B's database and enumerate its catalog (data itself stayed unreadable via table ownership).
Provisioning now closes the connection-level boundary NFR-SEC-01 mandates: `createRoleAndDatabase` runs an
idempotent `REVOKE CONNECT ON DATABASE <db> FROM PUBLIC` + `GRANT CONNECT ... TO <owner>` (`lockdownDatabaseSQL`,
unit-tested; SPEC-01 §6.2a). The `Unsafe()` escape hatch is now machine-enforced: a golangci-lint `forbidigo`
rule fails `mise run lint` in CI if any package outside `internal/provision`/`internal/migrate` calls
`tenant.DB.Unsafe()` (proven to catch a violation and to permit the allowed packages). The router-driven
cross-tenant endpoint matrix (A's credentials against B's IDs over every route, 404/403) plugs into this
two-tenant fixture when EPIC-04 lands. ADR-0018; SPEC-01 §6/§9 and SPEC-09 §1 updated with the code.

## EPIC-03 · Control plane services — ✅ 34/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-03.1 | User accounts and sessions | 5 | ✅ Done | FR-ACC-01, SPEC-09 §3 |
| STORY-03.2 | OIDC login | 5 | ✅ Done | FR-ACC-01 |
| STORY-03.3 | Tenant membership and roles | 5 | ✅ Done | FR-ACC-02/06, SPEC-02 §4 |
| STORY-03.4 | API keys | 5 | ✅ Done | FR-ACC-04/05 |
| STORY-03.5 | Tenant settings with JSON-schema validation | 3 | ✅ Done | FR-TEN-08, SPEC-02 §5 |
| STORY-03.6 | Audit log | 3 | ✅ Done | FR-ADM-05, SPEC-02 §6 |
| STORY-03.7 | Usage counters | 3 | ✅ Done | FR-ADM-06, SPEC-10 §6 |
| STORY-03.8 | Platform admin impersonation | 3 | ✅ Done | FR-ACC-07 |
| STORY-03.9 | Rate limiting | 2 | ✅ Done | NFR-SEC-07, SPEC-07 §1 |

**Delivered (STORY-03.1):** `internal/cp/auth` — control-plane-only email/password auth and server-side
sessions (FR-ACC-01, SPEC-09 §3), touching no tenant data (C-3). Passwords are hashed with argon2id
(t=3, m=64 MiB, p=4, per-hash random salt) in PHC-encoded form on `users.password_hash`, verified in
constant time; the plaintext is never stored or logged and a length floor is enforced (the breach-list
check is a follow-up hook). Signup rejects duplicate emails via the unique violation; Login collapses
unknown-email and wrong-password into one opaque error, and the lockout policy (10 failures / 15 min,
backed by `users.failed_login_count`/`locked_until`) refuses a locked account before the password is
checked and still refuses the correct password inside the window. Sessions live in a new control-plane
`sessions` table: the 128-bit cookie id is stored only as its sha256 (`token_hash`) so a leaked snapshot
cannot be replayed, `idle_expires_at` enforces and slides the 12 h idle timeout, and logout sets
`revoked_at`. The cookie is HttpOnly + SameSite=Lax (Secure in production) and the raw token is never
returned in a body; CSRF is a per-session double-submit token (`sessions.csrf_token`) required on mutating
methods and compared in constant time. `Handlers` (Signup/Login/Logout) and the `RequireSession`/`CSRF`
middleware are real `http.Handler`s exercised with `net/http/httptest`; mounting them on the public router
is STORY-04.1 (mirroring the EPIC-02 deferrals). Schema via goose control migration
`00004_users_auth_and_sessions.sql` mirrored into `schemas/control_plane.sql` so the drift guard stays
green. Unit tests (argon2id round-trip/salt uniqueness, token hashing/CSRF match, lockout policy, service
branch logic via a fake DB, middleware cookie/CSRF flows) + e2e golden path over the real control-plane
Postgres (signup → login → session lookup → logout, proving the stored hash is argon2id and the token is
stored as its sha256) and the lockout-after-10-failures path. ADR-0019.

**Delivered (STORY-03.2):** OIDC login in the same `internal/cp/auth` package (FR-ACC-01, SPEC-02 §3,
SPEC-09 §3), control-plane-only (C-3). A configurable provider (`OIDC_ISSUER`/`OIDC_CLIENT_ID`/
`OIDC_CLIENT_SECRET`/`OIDC_REDIRECT_URL`/`OIDC_JIT_PROVISIONING` via `internal/config`) drives the
authorization-code + PKCE flow: `AuthCodeURL` mints a per-attempt state, nonce, and PKCE verifier and
returns the provider URL with the S256 challenge; `Callback` compares `state` in constant time *before*
any token exchange, then verifies the id_token (signature via JWKS, issuer, audience, expiry, nonce) with
`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (the only file touching those libraries is
`oidc_provider.go`, behind `Exchanger`/`Verifier` interfaces, so the flow logic is stubbable — NFR-MNT-01).
Link/JIT act only on a provider-`email_verified` claim: an unverified email is refused. Resolution order is
existing `(issuer, subject)` identity → existing user by verified email (linked into a new `user_identities`
row) → JIT create when enabled, else refuse. JIT users are password-less (`password_hash` null), reachable
only via OIDC. On success a session is minted through the SAME store as password login (no fork), setting
the identical session cookie + CSRF response. New schema via goose control migration
`00005_oidc_identities.sql` (a `user_identities` table keyed by `(issuer, subject)` and a
`users.email_verified` column), mirrored into `schemas/control_plane.sql` so the drift guard stays green.
`OIDCHandlers` (Start/Callback) are real `http.Handler`s carrying the per-attempt state in a short-lived
HttpOnly cookie, unit-tested with `httptest`; router wiring is STORY-04.1. Unit tests (AuthCodeURL
PKCE/state/nonce, callback state/nonce/verified-email/JIT/link branches via a fake DB, the go-oidc
provider verifying a signed id_token and rejecting a bad nonce against a stub IdP, config, handlers) + e2e
golden path over the real control-plane Postgres with an in-process stub IdP (JIT creation on first login,
link-by-verified-email on a subsequent login with no duplicate user, and state/nonce mismatch rejected
without minting a session). ADR-0020.

**Delivered (STORY-03.3):** tenant membership, roles, and the role matrix in the same `internal/cp/auth`
package (FR-ACC-02/06, SPEC-02 §4), control-plane-only (C-3). The four spec roles (owner/admin/editor/
viewer) and the six permissions of the SPEC-02 §4 table are encoded once in `roles.go` as a `roleMatrix`
that fails closed (an unknown or zero role grants nothing; `ParseRole` rejects any invented role); a
matrix-vs-spec unit test pins every cell. `MembershipService` (`membership.go`) does members CRUD against
the existing `tenant_members` table — `AddMember` (invalid role and duplicate rejected), `ListMembers`
(joined to `users.email`, ordered), `SetMemberRole`, `RemoveMember` — with the "owner cannot remove or
demote the last owner" invariant enforced *atomically in one guarded SQL statement* per mutation (a
correlated `count(*) ... where role='owner' <= 1` guard, so it holds under concurrency without a
read-modify-write race); a zero-row result is disambiguated into `ErrNotMember` vs `ErrLastOwner`.
`AuthzService.RequireRole(perm)` (`authz.go`) is a real `http.Handler` middleware keyed off the
authenticated session user (STORY-03.1 `SessionFrom`) and the *resolved* tenant (`tenant.TenantIDFromCtx`,
never a client parameter — FR-ACC-03): it looks up the member's role and the platform-admin flag in one
query, 401s with no session/tenant, 403s a non-member or an under-privileged role, and lets platform
admins act on any tenant (FR-ACC-07). No schema change was needed (`tenant_members`/`tenant_role` already
existed) and no new design decision was made, so no migration and no ADR. Router wiring is deferred to
STORY-04.1, which only attaches `RequireRole`. Unit tests (matrix-vs-spec, ParseRole, membership branch
logic + last-owner invariant via a fake DB, and a table-driven role × permission middleware test with
`httptest`) + an e2e golden path over the real control-plane Postgres (add each role, list, change a role,
remove, last-owner removal AND demotion rejected, and the full role × route matrix through the real
`RequireRole` middleware). The real Postgres run also caught an enum-vs-text cast bug the fake missed
(guard comparisons now cast `$3::tenant_role`).

**Delivered (STORY-03.4):** API keys in the same `internal/cp/auth` package (FR-ACC-04/05, SPEC-02 §3,
SPEC-07 §2, SPEC-09 §3), control-plane-only (C-3). Scopes are a closed typed set — exactly `query`,
`ingest`, `admin` (`scope.go`) — with `ParseScope`/`ParseScopes` rejecting any invented or empty scope so
no capability-less or off-spec key can be minted or authenticate. The wire format (ADR-0021) is
`Authorization: Bearer rk_<prefix>_<secret>`: an `rk_` scheme marker, an 8-char **hex** prefix stored in
the clear (`key_prefix`, indexed lookup + display) — hex deliberately, because base64url includes the `_`
separator and a base64url prefix could be truncated on parse (a real bug the format test caught) — and a
32-byte base64url secret body. Only the sha256 of the FULL presented value is stored (`key_hash`, reusing
the session-token `hashToken`); `Create` returns the plaintext once and it is never persisted or logged
(FR-ACC-05, C-4). `APIKeyService` (Create/List/Revoke) writes/reads the existing `api_keys` table: List
shows prefix, scopes, and last-used (never the secret); Revoke stamps `revoked_at`, scoped to the tenant,
idempotent, `ErrKeyNotFound` for an unknown id. `APIKeyVerifier`/`RequireScope` authenticate a Bearer key
by `(key_prefix, key_hash)` with `revoked_at is null` so revocation is immediate; unknown/tampered/revoked/
malformed collapse to one opaque `ErrInvalidKey` (→ 401, no enumeration oracle), `expires_at` is checked in
Go (`ErrKeyExpired`, → 401 at the edge), `last_used_at` is stamped at most once per minute (throttled,
non-fatal), and the middleware injects the key's tenant into the request context (FR-ACC-03 — derived from
the credential, never a client parameter) and 403s an out-of-scope route. No schema/migration change was
needed (the `api_keys` table already matched the spec), so the drift guard stays green. Unit tests (scope
parsing, secret format/uniqueness, verifier branch logic — malformed/unknown/tampered/expired/golden and the
last-used throttle — and service validation via a fake DB + `httptest` middleware) + an e2e golden path over
the real control-plane Postgres (create returns the secret once and it is not stored; authenticate stamps
last-used and resolves the tenant; list shows prefix/scopes/last-used; revoke → immediately rejected and
idempotent; expired key rejected; out-of-scope refused). Router wiring is STORY-04.1. ADR-0021.

**Delivered (STORY-03.6):** the audit log subsystem — a new `internal/cp/audit` package (FR-ADM-05, SPEC-02
§6), control-plane-only (C-3). `audit.Record` is the sanctioned append-only writer: one `insert into audit_log`
carrying actor (user or API key), tenant, and target, defaulting nil details to `{}` and refusing an empty
action; details hold non-secret metadata only. `Service.List` is the reader — always tenant-scoped (fails
closed on an empty tenant so the whole log can never be fetched unscoped), newest-first by `id`, page size
defaulting to 50 / capped at 200 with `before=<id>` keyset pagination. `Handlers.List` serves
`GET /admin/audit?tenant=` (400 without a tenant param, malformed limit/before ignored). Because a platform
admin reads *across* tenants (FR-ACC-07) the tenant is a query parameter, not the resolved-credential tenant,
so the existing tenant-scoped `RequireRole` does not fit; STORY-03.6 adds a distinct tenant-less
`AuthzService.RequirePlatformAdmin` middleware (401 no session, 403 non-admin/unknown user, else pass) that
also gates STORY-03.8 impersonation and the platform-admin UI. No migration/schema change was needed
(`audit_log` already exists) so the drift guard stays green, and the reader returns every row regardless of
writer, so tenant.\* and settings.update history is already queryable. Per-action write wiring for the
remaining SPEC-02 §6 events adopts `audit.Record` with each action's handler (member.\*/apikey.\* in
STORY-04.1, source.\* in EPIC-04/06, job.cancel in EPIC-09, admin.impersonate in STORY-03.8), mirroring how
tenant.\* is written at its orchestration layer; the existing direct inserts in `provision`/`tenants` converge
onto `Record` as they are next touched (ADR-0022 anticipated this). Router wiring is STORY-04.1. Unit tests
(Record validation/defaults/error propagation, reader tenant-required/ordering/limit-clamp, handler
param-parsing, and the platform-admin middleware 401/403/200 matrix via `httptest`) + an e2e golden path over
the real control-plane Postgres (a genuine settings.update writes a row; a platform admin reads it back with
actor/tenant/target; a non-admin is 403, no session 401, and a missing tenant param 400). ADR-0023.

**Delivered (STORY-03.7):** the usage accounting subsystem — a new `internal/cp/usage` package
(FR-ADM-06, SPEC-10 §6, SPEC-02 §2), control-plane-only (C-3). `usage.Counter` is the sanctioned,
non-blocking write surface other subsystems adopt (the usage analogue of `audit.Record`):
`Add(tenantID, Delta{...})` merges a per-`(tenant, UTC-day)` `Delta` — queries, docs ingested, chunks
embedded, embed tokens, LLM in/out tokens, the six `usage_daily` columns — under a mutex with no I/O, so
it is safe on a request/job hot path. `Counter.Run` flushes every 30 s (SPEC-10 §6) and drains once more on
shutdown so the last window is not lost; each `(tenant, day)` bucket is written with one **accumulating**
upsert (`insert ... on conflict (tenant_id, day) do update set col = usage_daily.col + excluded.col`), so
repeated flushes — and multiple API/worker replicas each with their own counter — sum rather than overwrite
(a pinned-SQL unit test asserts every column accumulates). A failed flush merges the un-flushed buckets back
into the buffer and retries on the next tick, so counts are at-least-once within a process lifetime; an
empty tenant or zero delta is dropped (fail closed), never written to a blank row. `Service.List` is the
tenant-scoped reader (fails closed on an empty tenant, defaults a range-less read to the last 30 days,
rejects an inverted range), and `Handlers.List` serves `GET /v1/usage?from&to` (SPEC-07) taking the tenant
from the resolved context (FR-ACC-03, never a parameter; 401 if unresolved, 400 on a malformed/inverted
range), mirroring the settings handlers. No migration/schema change was needed (`usage_daily` already
matches SPEC-02 §2) so the drift guard stays green. Per-producer wiring (queries in EPIC-08 retrieval,
docs/chunks/embed tokens in EPIC-05 ingestion, LLM tokens in EPIC-08 answering) adopts `Counter.Add` with
each producer, and the process-lifetime `Run` is wired into `ragctl serve`/`work` in EPIC-04/09 — mirroring
how audit deferred its per-action write wiring. Router wiring is STORY-04.1. Unit tests (delta merge per
tenant/day, day truncation, zero/empty-tenant drop, flush clears buffer / error retains counts, concurrent
adds counted exactly once, the accumulating-upsert SQL contract, reader tenant-required/ordering/range
defaulting, handler tenant-from-context / date parsing / 401 / 400, and the periodic-flush-then-final-drain
loop) + an e2e golden path over the real control-plane Postgres (two flush cycles accumulate on one
`usage_daily` row, `GET /v1/usage` returns the resolved tenant's rows, no-tenant refused 401). ADR-0024.

**Delivered (STORY-03.8):** platform-admin impersonation in the `internal/cp/auth` package (FR-ACC-07,
SPEC-02 §4/§6, SPEC-09 §3), control-plane-only (C-3). An impersonation is an explicit, audited **grant**,
never a silent identity swap: `ImpersonationService.Start` writes an `impersonation_sessions` row recording
BOTH the real admin actor (`admin_user_id`) AND the impersonated principal (`tenant_id` +
`impersonated_user_id`), so every action taken under it stays attributable back to the admin (the whole point
of FR-ACC-07's "audit-logged" clause). The grant is time-bounded (`expires_at`, a 1 h default) and revocable
(`End` stamps `ended_at`); `Impersonation.Active(now)` is the single fail-closed predicate — an ended or
expired grant is inactive — and missing arguments / unknown ids are refused (`ErrNoImpersonation` → 404).
Only platform admins may start it: the `Start`/`End` handlers assume `RequireSession` +
`RequirePlatformAdmin` (the tenant-less middleware STORY-03.6 introduced, reused not reinvented) and read the
acting admin from the **session**, never a body field (FR-ACC-03), so a caller cannot forge the actor. Start
writes an `admin.impersonate` audit event and End an `admin.impersonate.end` companion, both through the
sanctioned `audit.Record` writer via an injected `AuditFunc` seam, carrying actor = the real admin, target =
the impersonated user, tenant = the impersonated tenant, and `details.impersonation=true` (SPEC-02 §4) with
non-secret ids only (C-3). New schema via goose control migration `00006_impersonation_sessions.sql`,
mirrored into `schemas/control_plane.sql` so the drift guard stays green. Request-time application of a live
grant (treating a request as the impersonated user, the UI banner) rides the same `RequirePlatformAdmin` gate
in STORY-04.1/EPIC-11; this story delivers the grant + audit primitive. Router wiring is STORY-04.1. Unit
tests (Start argument validation / grant carries both identities / time bound / audit event, End
stamps+audits / unknown-grant fails closed, the `Active` expiry+ended matrix, and handler branches — session
admin used not the body, 401/400/404/204 — via fakes + `httptest`) + an e2e golden path over the real
control-plane Postgres (a non-admin refused 403 with no grant written, an admin's grant carrying both
identities and persisted, an `admin.impersonate` row attributed to the admin with `details.impersonation`,
and End stamping the grant + writing `admin.impersonate.end`). ADR-0025.

**Delivered (STORY-03.9):** rate limiting — a new `internal/cp/ratelimit` package (NFR-SEC-07, SPEC-07 §1),
control-plane-only (C-3). The SPEC-07 §1 shape is realised as a token bucket **per API key and per tenant**,
both steady-rate at the tenant's `settings.limits.qps`: `bucket` is a lazily-refilled token bucket (refilled
on demand from elapsed time against an **injected clock** — no per-bucket goroutine, deterministic tests with
no real sleeps), `Limiter` holds one bucket per `key:<id>`/`tenant:<id>` in a mutexed map and sweeps idle
buckets (`Run`/`idleTTL`, 10 min) so memory does not grow with ever-seen keys (a re-created bucket starts full,
so eviction only reclaims memory, never tightens a limit). `Middleware.Handler` requires a request to pass
**both** buckets — the per-tenant bucket is the aggregate ceiling (looser burst, `RATE_LIMIT_TENANT_BURST`
default 2×) and the per-key bucket caps a single credential (`RATE_LIMIT_KEY_BURST` default 1×), which is what
makes per-key isolation observable rather than the tenant bucket always biting first (ADR-0026). The limit key
is derived from the resolved tenant + authenticated key id in context (FR-ACC-03, never a client parameter):
`auth.RequireScope` now also injects the key id (`auth.WithKeyID`/`KeyIDFromCtx`); a session request carries no
key and is limited by the tenant bucket only. On refusal the middleware sets `Retry-After` (whole seconds,
rounded up) plus `RateLimit-Limit`/`RateLimit-Remaining`/`RateLimit-Reset`, writes the SPEC-07 §1 error envelope
(`rate_limited`), and never reaches the inner handler. **Fail closed:** a settings-lookup error → 429 (limiting
is never silently disabled by a backing-store hiccup), a missing/malformed qps → a configured floor
(`RATE_LIMIT_DEFAULT_QPS`, default 10 = the SPEC-02 §5 default), a request with no resolved tenant → 401. The
"metrics" AC is an optional `prometheus.Counter` (`Rejected`) incremented on each 429 (a nil counter is safe).
No migration/schema change was needed (the limit is read from the existing `tenants.settings.limits.qps`,
SPEC-02 §5), so the drift guard stays green. The store is **in-process** (the spec does not require a shared one):
each replica keeps its own buckets, so the effective ceiling scales with replica count — accepted for
abuse-protection limiting and documented in ADR-0026 as the single seam to swap for a distributed store if a
hard global cap is ever required. Router wiring is STORY-04.1, which mounts this middleware into the chain
(mirroring how the other 03.x middleware defer wiring). Unit tests (bucket burst/refill/cap, per-key/per-tenant
isolation + ceiling, 429 headers, session-no-key, no-tenant/lookup-error fail-closed, settings qps extraction
incl. float decode + default fallback, metric increment, eviction loop; config knobs + defaults) + an e2e
golden path over the real control-plane Postgres driving the real `RequireScope` → rate-limit chain (a request
over the per-key limit → 429 with `Retry-After`/`RateLimit-*`, and a second key of the same tenant unaffected —
per-key isolation). ADR-0026.

## EPIC-04 · Public API surface — 🚧 13/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-04.1 | HTTP server, routing, middleware chain | 5 | ✅ Done | SPEC-07 §1, ADR-0027 |
| STORY-04.2 | OpenAPI generation and contract tests | 5 | ✅ Done | SPEC-07 §3, ADR-0028 |
| STORY-04.3 | Sources endpoints | 3 | ✅ Done | FR-SRC-01/14 |
| STORY-04.4 | Documents endpoints | 3 | 🔲 Todo | FR-SRC-02, FR-ADM-03 |
| STORY-04.5 | Jobs endpoints | 2 | 🔲 Todo | FR-ADM-02 |
| STORY-04.6 | Admin tenant endpoints | 3 | 🔲 Todo | FR-TEN-01/05/07 |

**Delivered (STORY-04.1):** the public HTTP server — a new dependency-injected `internal/api` package plus its
wiring in `internal/cli` (SPEC-07 §1, ADR-0027). `api.New(Deps)` assembles a Go 1.22 `net/http.ServeMux` (no
new dependency): the operational endpoints open and unauthenticated (`GET /healthz`, `GET /readyz` with the
control-plane ping probe, `GET /metrics`), the open auth routes (signup/login/logout, oidc start/callback), the
platform-admin surface (`GET /admin/audit`, `POST`/`DELETE /admin/impersonations[/{id}]`) behind
`RequireSession → RequirePlatformAdmin` with CSRF on the mutations, and the per-tenant surface (`GET /v1/usage`)
behind `RequireScopeAdmin → RateLimit`. Middleware and handlers are injected as values so the whole chain is
unit-testable with stubs and the single control-plane pool (never a tenant pool — C-3) is opened once in
`cli.buildAPIServer`; a nil middleware is a pass-through and a nil handler is a not-implemented seam returning the
`not_found` envelope, so a partially-wired server boots and fails closed. The global chain is, outer → inner,
`obs.Middleware (request-id/logging/tracing/metrics) → Recover → CORS → [route]`: `obs.Middleware` is outermost so
`X-Request-Id` stamps every response for log correlation, `Recover` turns any downstream panic into a `500`
envelope (logged server-side with the request id, never leaking the panic value/stack). The credential-keyed rate
limiter runs *inside* per-route auth (a deliberate, intent-preserving divergence from SPEC-07 §1's abstract "rate
limit → auth", documented in ADR-0027) because the STORY-03.9 bucket keys off the resolved credential + tenant
(FR-ACC-03). Every router-mounted middleware/handler was converged onto the one SPEC-07 §1 object envelope
`{"error":{"code","message"}}` (fixing an anon `/admin/audit` bare-string body an e2e caught). `ragctl serve` is
the sole entrypoint (ADR-0009): it builds the router, starts the rate-limiter idle sweep and the usage-counter
flush loop on the signal-cancelled context, and shuts the HTTP server down gracefully. Sources/documents/jobs/
settings/members/api-keys and the admin-tenant routes are intentionally unregistered **seams** (not stubs) that
04.3–04.6 slot into; session-based `/v1` tenant resolution is likewise deferred (the only mounted tenant-scoped
route derives its tenant from the API key, so no resolver is on the hot path). Unit tests (envelope shape +
request id, `chain` order, `Recover` panic→500 and pass-through, healthz/readyz open, login reaches its handler,
audit guarded by platform-admin with 403 + envelope, the usage `scope → rate-limit → handler` order, over-limit
`429`, unauthenticated `401`, unknown route `404` envelope, seam groups `404`) + an e2e golden path over the real
control-plane Postgres (`test/e2e/api_router_e2e_test.go`: the assembled router over a real listener — healthz/
readyz open, anon `/admin/audit` `401` object envelope with `X-Request-Id`, seed a real `settings.update` row,
login through the mounted route, platform admin reads the audit log through the full chain, non-admin `403`). No
migration/schema change (this story only wires existing services) so the drift guard stays green. ADR-0027.

**Delivered (STORY-04.2):** OpenAPI generation and contract tests (SPEC-07 §3, ADR-0028). The OpenAPI 3.1
document is built in `internal/api/openapi.go` from a single `liveRoutes()` table (mirroring the routes
STORY-04.1's router mounts) and the `ErrorCodes()` list derived from the SPEC-07 §1 `Code*` constants — so
the spec is genuinely code-derived: the `ErrorEnvelope` component schema matches the `errorEnvelope` Go type
and its `code` enum *is* `ErrorCodes()`, and neither can drift from what `WriteError` emits. It is served as
JSON at `GET /v1/openapi.json` (open, no auth — a public description drives client/SDK generation) via
`OpenAPIHandler()`, mounted alongside the operational endpoints, and marshalled to the checked-in
`api/openapi.yaml` by `mise run openapi` (a new `ragctl openapi` Kong subcommand that needs no DB/config, so it
regenerates offline and keeps `mise.toml` minimal — one task file, no tool pin; single-entrypoint per
ADR-0009). **Divergence from SPEC-07 §3's literal "oapi-codegen or swag", recorded in ADR-0028:** no
code-generation toolchain was added — `oapi-codegen` is spec-first (wrong direction) and `swag` is
annotation-driven codegen, both heavy for a small, mostly-seam surface. The only new import is
`gopkg.in/yaml.v3` (promoted from transitive to direct); `santhosh-tekuri/jsonschema/v6` is reused. SPEC-07 §3
updated to describe the realised approach. **Contract enforcement is two-pronged:** a drift-guard unit test
fails CI when `api/openapi.yaml` is stale (regenerate with `mise run openapi`), and a jsonschema contract test
(unit + an e2e golden path over the real control-plane Postgres, `test/e2e/openapi_e2e_test.go`) drives *real*
error responses from the assembled router (a 401 from the scope gate, a 404 from the unknown-route fallback)
and validates them against the `ErrorEnvelope` schema extracted from the *served* spec, with a negative control
(a bare-string `{"error":"…"}` body) proving the check has teeth. TDD throughout (tests written and watched
red before the builder existed). No migration/schema change and no tenant data touched (C-3), so the drift
guard stays green; `internal/api` stays outside the coverage gate (consistent with ADR-0027) but the new code
is unit- + e2e-covered. The route table is the growable seam 04.3–04.6 append to. ADR-0028.

**Delivered (STORY-04.3):** the sources API — a new `internal/cp/sources` package (FR-SRC-01/14, SPEC-07 §2/§2a,
ADR-0029) plus its seven routes wired into `internal/api` (`New` + `liveRoutes()`) behind
`RequireScopeAdmin → RateLimit`. Sources are control-plane registry data (C-3), so the package operates on the
control-plane pool via a `Store`/PoolDB — it never opens a tenant database (ADR-0003) — and every operation is
scoped to the tenant resolved from the API key (FR-ACC-03, no `tenant_id` parameter). `GET /v1/sources`
(`?limit&cursor` → `{items,next_cursor}` keyset pagination), `POST` (create), `GET/PATCH/DELETE /{id}`
(PATCH covers pause/resume via `status`, restricted to active/paused; delete marks the source `deleting` and
enqueues a `delete_source` job), `POST /{id}/sync` and `POST /{id}/test`. **Concurrent-sync 409** is enforced by
the *existing* `jobs_one_active_sync_per_source` partial unique index — the sync handler writes a queued
`sync_source` mirror row (the row the EPIC-09 worker will consume) and maps the unique violation to `conflict`;
the **Idempotency-Key** is stored in `jobs.payload` and an active matching sync is replayed rather than
conflicting (SPEC-07 §1). Two dependencies are **injected seams**, not built here: the connector framework
(`Validator`: `ValidateConfig`/`Test`, EPIC-06 STORY-06.1) is nil today — `/test` returns the `not_found` seam
envelope (mirroring STORY-04.1) and create/update run generic validation only (kind/name/config-shape), with the
kind-specific `ValidateConfig` slotting in when the port is wired (NFR-MNT-01); and the River worker that
executes the queued jobs and performs the FR-SRC-12 cascade (EPIC-09 STORY-09.1/09.6). Credentials (FR-SRC-10)
are deferred to STORY-06.2: a `credentials` field in the body is rejected `400` (fail closed, no plaintext on the
write path — C-4) and no response ever returns credentials. No migration/schema change (the `sources`/`jobs`
tables and the unique index already existed), so the drift guard stays green; `api/openapi.yaml` regenerated via
`mise run openapi` so the served spec, the drift guard and the contract tests grow with the new routes
(ADR-0028). TDD throughout (unit tests written and watched red before the service/handlers existed). Unit tests
(`internal/cp/sources`: validation branches, duplicate-name/404/409 mapping, pause/resume, delete idempotency,
sync 409 + idempotent replay, `/test` seam, cursor pagination; `internal/api`: the seven routes run
`scope-admin → rate-limit → handler`) + an e2e golden path over the real control-plane Postgres
(`test/e2e/sources_e2e_test.go`: create→list→get→patch→sync→idempotent-replay→409→delete→test-seam through the
assembled API-key chain, asserting the queued jobs and `deleting` status land in the real tables and that no
credentials are echoed). ADR-0029; ISSUE-0002. _(Pre-existing, unrelated to this story: `mise run coverage` and
full `mise run lint` are red in the local environment for a golangci-lint/Go-toolchain drift on two EPIC-03
files and env-sensitive `internal/cli` tests — verified identical on a clean checkout; no gated package was
touched.)_

## EPIC-05 · Ingestion pipeline — 🔲 0/42 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-05.1 | Document and version store | 5 | 🔲 Todo | FR-ING-02, ADR-0008, SPEC-03 |
| STORY-05.2 | Go parsers: HTML, Markdown, text, CSV, JSON | 5 | 🔲 Todo | FR-ING-01, SPEC-05 §2 |
| STORY-05.3 | Parsing sidecar (Python) and Go client | 8 | 🔲 Todo | FR-ING-11, ADR-0006 |
| STORY-05.4 | Structure-aware chunker | 5 | 🔲 Todo | FR-ING-03/04, SPEC-05 §3 |
| STORY-05.5 | Embedding provider interface and implementations | 5 | 🔲 Todo | FR-ING-05, NFR-MNT-02 |
| STORY-05.6 | Sink implementation and commit semantics | 5 | 🔲 Todo | FR-ING-07, NFR-REL-02, SPEC-05 §5 |
| STORY-05.7 | Job stats and error capture | 2 | 🔲 Todo | FR-ING-10 |
| STORY-05.8 | Reindex job with table swap | 5 | 🔲 Todo | FR-ING-09, SPEC-03 §5, SPEC-05 §7 |
| STORY-05.9 | Garbage collection job | 2 | 🔲 Todo | SPEC-03 §4 |

## EPIC-06 · Connector framework and upload connector — 🔲 0/13 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-06.1 | Connector interface, registry, config validation | 5 | 🔲 Todo | FR-SRC-13, NFR-MNT-01, SPEC-04 §1 |
| STORY-06.2 | Credential encryption and handling | 3 | 🔲 Todo | FR-SRC-10, SPEC-04 §6 |
| STORY-06.3 | Upload connector and ingest_document job | 5 | 🔲 Todo | FR-SRC-02, SPEC-04 §5 |

## EPIC-07 · Web crawl, sitemap and API connectors — 🔲 0/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-07.1 | Web crawler core | 8 | 🔲 Todo | FR-SRC-03/04, SPEC-04 §2 |
| STORY-07.2 | SSRF protection and egress rules | 3 | 🔲 Todo | NFR-SEC-04, SPEC-09 §4 |
| STORY-07.3 | HTML content extraction quality | 5 | 🔲 Todo | FR-SRC-05 |
| STORY-07.4 | Conditional fetch and change detection | 3 | 🔲 Todo | FR-ING-02 |
| STORY-07.5 | Sitemap connector | 3 | 🔲 Todo | FR-SRC-06 |
| STORY-07.6 | HTTP API connector: auth and pagination | 8 | 🔲 Todo | FR-SRC-07, SPEC-04 §4 |
| STORY-07.7 | HTTP API connector: templating and incremental sync | 5 | 🔲 Todo | FR-SRC-07/08 |
| STORY-07.8 | Source "test connection" for all kinds | 2 | 🔲 Todo | FR-SRC-14 |
| STORY-07.9 | Connector documentation | 2 | 🔲 Todo | — |

## EPIC-08 · Retrieval and answering — 🔲 0/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-08.1 | Hybrid retrieval query | 8 | 🔲 Todo | FR-RET-01/02/08, ADR-0007, SPEC-06 §2 |
| STORY-08.2 | Retrieve endpoint | 2 | 🔲 Todo | FR-RET-08 |
| STORY-08.3 | Reranker interface and providers | 5 | 🔲 Todo | FR-RET-03 |
| STORY-08.4 | LLM provider interface | 5 | 🔲 Todo | NFR-MNT-02, NFR-REL-04 |
| STORY-08.5 | Prompt assembly, citations and grounding refusal | 8 | 🔲 Todo | FR-RET-04/05, SPEC-06 §4–5 |
| STORY-08.6 | Query endpoint with streaming | 5 | 🔲 Todo | FR-RET-06, SPEC-06 §6 |
| STORY-08.7 | Conversation history and question rewrite | 3 | 🔲 Todo | FR-RET-07 |
| STORY-08.8 | Query log and feedback | 3 | 🔲 Todo | FR-RET-09/10 |

## EPIC-09 · Jobs, scheduling and maintenance — 🔲 0/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-09.1 | River integration and worker binary | 5 | 🔲 Todo | FR-ING-08, ADR-0005, SPEC-08 §1 |
| STORY-09.2 | Job status mirroring to `jobs` table | 3 | 🔲 Todo | FR-ADM-02, SPEC-08 §3 |
| STORY-09.3 | Scheduler for cron sources and daily GC | 5 | 🔲 Todo | FR-SRC-11, SPEC-08 §2 |
| STORY-09.4 | Cancellation and uniqueness | 3 | 🔲 Todo | SPEC-08 §4 |
| STORY-09.5 | Per-tenant concurrency caps and fairness | 3 | 🔲 Todo | — |
| STORY-09.6 | Delete-source job | 2 | 🔲 Todo | FR-SRC-12 |

## EPIC-10 · Security, observability, operations — 🔲 0/26 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-10.1 | Metrics catalogue and dashboards | 5 | 🔲 Todo | FR-OBS-02, SPEC-10 §2/5 |
| STORY-10.2 | Alert rules | 2 | 🔲 Todo | SPEC-10 §5 |
| STORY-10.3 | Distributed tracing end to end | 3 | 🔲 Todo | FR-OBS-03 |
| STORY-10.4 | DEK rotation command | 3 | 🔲 Todo | NFR-SEC-03, SPEC-09 §2 |
| STORY-10.5 | Backups and PITR verification | 3 | 🔲 Todo | NFR-REL-03 |
| STORY-10.6 | Security scanning in CI and dependency policy | 2 | 🔲 Todo | SPEC-09 §6 |
| STORY-10.7 | Load and isolation testing | 5 | 🔲 Todo | NFR-PERF-01, SRS §8 |
| STORY-10.8 | Runbooks | 3 | 🔲 Todo | — |

## EPIC-11 · Admin UI (reference) — 🔲 0/34 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-11.1 | App shell, auth, tenant switcher | 5 | 🔲 Todo | — |
| STORY-11.2 | Sources list/create/edit with per-kind forms and test-connection | 8 | 🔲 Todo | FR-ADM-01 |
| STORY-11.3 | Jobs list and detail with cancel | 5 | 🔲 Todo | FR-ADM-02 |
| STORY-11.4 | Documents and chunks browser | 5 | 🔲 Todo | FR-ADM-03 |
| STORY-11.5 | Members, API keys, settings pages | 5 | 🔲 Todo | — |
| STORY-11.6 | Query playground with citations and feedback | 3 | 🔲 Todo | — |
| STORY-11.7 | Platform admin: tenants list, enrol, suspend, delete | 3 | 🔲 Todo | — |

## EPIC-12 · Evaluation harness and quality — 🔲 0/13 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-12.1 | Eval cases CRUD and import (CSV) | 3 | 🔲 Todo | FR-ADM-04 |
| STORY-12.2 | `ragctl eval run` with recall@k, grounded rate, latency | 5 | 🔲 Todo | SPEC-06 §8 |
| STORY-12.3 | LLM-as-judge correctness scoring (optional flag) | 3 | 🔲 Todo | — |
| STORY-12.4 | Eval report in admin UI and CI gate for settings changes | 2 | 🔲 Todo | — |

---

_EPIC-04 (Public API surface) is now under way: STORY-04.1 stands up the public router and mounts the accumulated
EPIC-03 middleware in the SPEC-07 §1 order (request ID/logging → recovery → CORS globally, with auth →
credential-keyed rate limit per route) plus every 03.x handler built behind it — the router-wiring seam every
EPIC-02/03 story deferred to. STORY-04.2 then generates the OpenAPI 3.1 spec from that router's route table,
serves it at `/v1/openapi.json`, and adds the drift-guard + jsonschema contract tests (SPEC-07 §3, ADR-0028).
STORY-04.3 then adds the sources
endpoints (`internal/cp/sources`) over the control-plane pool, with the connector framework and the job worker as
injected seams for EPIC-06/09 (ADR-0029). Suggested next: the remaining resource endpoints (04.4 documents, 04.5
jobs, 04.6 admin tenants) that slot into the `not_found` route seams STORY-04.1 left in place — each appends its
routes to `liveRoutes()` so the spec grows with them._
