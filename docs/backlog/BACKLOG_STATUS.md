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
| EPIC-04 | Public API surface | 21 | 21 | ✅ Complete |
| EPIC-05 | Ingestion pipeline | 42 | 42 | ✅ Complete |
| EPIC-06 | Connector framework and upload connector | 13 | 13 | ✅ Complete |
| EPIC-07 | Web crawl, sitemap and API connectors | 39 | 39 | ✅ Complete |
| EPIC-08 | Retrieval and answering | 39 | 15 | 🚧 In progress |
| EPIC-09 | Jobs, scheduling and maintenance | 21 | 0 | 🔲 Todo |
| EPIC-10 | Security, observability, operations | 26 | 0 | 🔲 Todo |
| EPIC-11 | Admin UI (reference) | 34 | 0 | 🔲 Todo |
| EPIC-12 | Evaluation harness and quality | 13 | 0 | 🔲 Todo |
| **Total** | | **337** | **179** | **53%** |

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

## EPIC-04 · Public API surface — ✅ 21/21 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-04.1 | HTTP server, routing, middleware chain | 5 | ✅ Done | SPEC-07 §1, ADR-0027 |
| STORY-04.2 | OpenAPI generation and contract tests | 5 | ✅ Done | SPEC-07 §3, ADR-0028 |
| STORY-04.3 | Sources endpoints | 3 | ✅ Done | FR-SRC-01/14 |
| STORY-04.4 | Documents endpoints | 3 | ✅ Done | FR-SRC-02, FR-ADM-03 |
| STORY-04.5 | Jobs endpoints | 2 | ✅ Done | FR-ADM-02 |
| STORY-04.6 | Admin tenant endpoints | 3 | ✅ Done | FR-TEN-01/05/07 |

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

**Delivered (STORY-04.4):** the documents API — a new `internal/documents` package (FR-SRC-02, FR-ADM-03,
SPEC-07 §2/§2b, ADR-0030) plus its five routes wired into `internal/api` (`New` + `liveRoutes()`). Unlike
sources, documents/versions/chunks are **tenant content** (`schemas/tenant.sql`, C-3), so — for the first time on
the request path — the routes reach a tenant database, and only through a `tenant.DB` from the resolver
(ADR-0003): the `Service` holds a `tenant.Resolver` (the sole source of a handle), the `TenantStore` runs the
SQL, and `buildAPIServer` now constructs `tenant.NewResolver` from the control pool + the **startup cipher it had
reserved since STORY-04.1**. The tenant is always the one resolved from the API key (FR-ACC-03, no `tenant_id`
parameter); no `tenant_id` column exists on tenant tables (C-1). Scopes follow SPEC-07 §2: `ingest` for
upload/delete, `query` for list/get, `admin` for the chunks debug endpoint. `GET /v1/documents`
(`?source&status&q&limit&cursor` → `{items,next_cursor}` keyset pagination), `GET /v1/documents/{id}`
(current-version metadata; `?content=true` adds the full normalised text), `DELETE /v1/documents/{id}` (soft
delete → status `deleted`; `live_chunks` already hides non-active docs), and `GET /v1/documents/{id}/chunks`
(current-version chunks for debugging — the opaque embedding vector is **never** returned) are fully served
against the tenant schema. `POST /v1/documents` validates the multipart upload here (**FR-SRC-02** type allowlist
— pdf/docx/md/html/txt/csv — and a configurable size ceiling, default 50 MB via the new `MAX_UPLOAD_BYTES`) and
enqueues a **real** `ingest_document` job in the control-plane `jobs` table; the **Idempotency-Key** is stored in
`jobs.payload` and replays the active ingest job (SPEC-07 §1). Two dependencies are **injected seams**, not built
here: object storage (`Storage` port, EPIC-06 STORY-06.x) is nil today, so the upload returns the `not_found`
seam envelope (mirroring STORY-04.1/04.3) until it is wired — and the ingest worker (EPIC-09) that consumes the
job. Critically, **no document row is created on upload**: an active document must have a non-null
`current_version` and there is no pending status (SPEC-03 §2 invariant 1), so the row and its first version are
built together by the ingest worker/document store (STORY-05.1, ADR-0008) in one transaction — the `202`
response carries the queued job as the client's handle. No migration/schema change (the
`documents`/`document_versions`/`chunks` tables and the `ingest_document` job kind already existed), so the drift
guard stays green; `api/openapi.yaml` regenerated via `mise run openapi` so the served spec, the drift guard and
the contract tests grow with the five routes (ADR-0028). TDD throughout (unit tests watched red before the
service/handlers existed). Unit tests (`internal/documents`: upload allowlist/size, cursor round-trips,
open-error mapping, nil-storage seam, enqueue payload + idempotent replay, delete not-found/read-only, handler
multipart parsing + error→envelope mapping; `internal/api`: the five routes run their scope gate → rate-limit →
handler) + an e2e golden path over a **real enrolled tenant database** and the real control-plane Postgres
(`test/e2e/documents_e2e_test.go`: seed a document/version/chunk in the tenant DB, then list→filter→get→
get-`?content`→chunks→delete through the assembled API-key chain, and a real `ingest_document` enqueue via a
test `Storage`, asserting the soft delete and the queued job land in the real tables and no embedding vector is
echoed). `TestOpenAPIContractGoldenPath`, `TestAPIRouterGoldenPath`, `TestSourcesGoldenPath` and the
`TestTenantIsolationSuite` (SPEC-01 §9, re-run because `internal/api` changed) all stay green. ADR-0030;
ISSUE-0003. _(Pre-existing, unrelated to this story: full `mise run test`/`lint`/`coverage` are red in the local
environment for a golangci-lint/Go-toolchain drift on two EPIC-03 audit files and env-sensitive `internal/cli`
tests under mise's `.env` injection — the latter pass once the leaked `CONTROL_PLANE_URL`/age-key env is cleared;
no gated package was touched.)_

**Delivered (STORY-04.5):** the jobs API — a new `internal/cp/jobs` package (FR-ADM-02, SPEC-07 §2/§2c, SPEC-08
§3/§4, ADR-0031) plus its three routes wired into `internal/api` (`New` + `liveRoutes()`) behind
`RequireScopeAdmin → RateLimit`. Jobs are the control-plane history/mirror view of the queue (ADR-0005), a
control-plane table (C-3), so — like sources (STORY-04.3), not documents — the package operates on the
control-plane pool via a `Store`/PoolDB and never opens a tenant database (ADR-0003); every operation is scoped
to the tenant resolved from the API key (FR-ACC-03, no `tenant_id` parameter). `GET /v1/jobs`
(`?status&kind&source&limit&cursor` → `{items,next_cursor}` keyset pagination on `(queued_at, id)`; status/kind
filters validated against the enums) and `GET /v1/jobs/{id}` return status, `attempt`, `stats`, timing and a
computed `duration_ms` for finished jobs (FR-ADM-02). **`POST /v1/jobs/{id}/cancel` realises SPEC-08 §4 against
what exists today** (there is no worker yet — EPIC-09): a **queued** job is cancelled *immediately and fully* —
the mirror row is flipped `queued`→`cancelled` in one guarded SQL statement (`… where status='queued'`, race-safe;
`finished_at` stamped), HTTP `200`, which satisfies FR-ADM-02's "cancel a queued job" literally because a queued
mirror row has no worker holding it and the mirror is authoritative. A **running** job's cancel is *cooperative*
(the worker observes `ctx.Done()` between documents and exits `cancelled`, committing nothing partial, SPEC-08
§4) — that is a River operation and River is EPIC-09, so it is an **injected `Canceller` seam**: nil today →
the `not_found` seam envelope (mirroring STORY-04.3 `/test` and STORY-04.4 upload); once wired it returns `202`
(cancellation requested) and the **worker middleware** writes the `running`→`cancelled` transition (SPEC-08 §3).
Flipping a running row in the API was **deliberately rejected** as a fake (it would race the real worker and
falsely claim the job stopped — AGENTS.md Integrity). A **terminal** job (succeeded/failed) → `409 conflict`; an
already-cancelled job is an idempotent `200`. No mirror column was added for the running-cancel signal (it belongs
in River, not the mirror). No migration/schema change (the `jobs` table, its `job_status`/`job_kind` enums, and
the `(tenant_id, queued_at desc)` index already existed), so the drift guard stays green; `api/openapi.yaml`
regenerated via `mise run openapi` so the served spec, the drift guard and the contract tests grow with the three
routes (ADR-0028). Auditing the cancel action (FR-ADM-05) rides EPIC-09 with the rest of the job lifecycle,
consistent with the STORY-03.6 plan. TDD throughout (unit tests watched red before the service/handlers existed).
Unit tests (`internal/cp/jobs`: tenant-required/scoped list+get, filter validation, cursor pagination, duration
computation, and the full cancel state machine — queued-effective-now, running-seam vs running-with-canceller,
terminal-409, already-cancelled-idempotent, unknown-404; handler branches via `httptest`) + an e2e golden path
over the real control-plane Postgres (`test/e2e/jobs_e2e_test.go`: seed queued/running/succeeded rows, then
list→status-filter→invalid-filter-400→get→get-missing-404→cancel-queued-200(+DB flip)→cancel-terminal-409→
cancel-running-404-seam(+running row unchanged) through the assembled API-key chain, plus FR-ACC-03 cross-tenant
isolation — a second tenant's job is neither listed nor gettable). `TestOpenAPIContractGoldenPath`,
`TestAPIRouterGoldenPath`, `TestSourcesGoldenPath`, `TestDocumentsGoldenPath` and `TestTenantIsolationSuite`
(SPEC-01 §9, re-run because `internal/api` changed) all stay green. ADR-0031; ISSUE-0004. _(Pre-existing,
unrelated to this story: `test/e2e/audit_e2e_test.go` trips one `revive` lint finding and the `internal/cli` mise
coverage run leaks env; both pre-date this change and no gated package was touched.)_

**Delivered (STORY-04.6):** the platform-admin tenant API — a thin HTTP layer in `internal/cp/tenants`
(`AdminService`/`AdminHandlers`/`AdminPoolStore`) plus its four routes wired into `internal/api` (`New` +
`liveRoutes()`) behind the existing `RequireSession → RequirePlatformAdmin` gate with CSRF on the mutations
(FR-TEN-01/05/07, SPEC-07 §2/§2d, ADR-0032). **This completes EPIC-04 (21/21).** The KEY move: the backend
already existed (STORY-02.3/02.4/02.5), so this **wires, it does not rebuild** — each route routes to the service
that owns it and adds no duplicate audit. `POST /admin/tenants` runs `provision.Provisioner.Provision`
**synchronously** (exactly the ADR-0016 precedent `ragctl enroll` set — the async River `provision_tenant`
*execution* is the one EPIC-09 seam) and records a `provision_tenant` mirror row, returning `{tenant, job_id}`
(`201`); because the tenant is active before the response returns, the mirror row is a truthful `succeeded`
record (ADR-0005 history view), **not** a perpetually-`queued` placeholder — a fake that was deliberately
rejected (AGENTS.md Integrity). `GET /admin/tenants` (`?limit&cursor` → `{items,next_cursor}` keyset pagination
on `(created_at, id)`) lists the registry over the control-plane pool (C-3); the view carries no connection
secrets (C-4). `PATCH /admin/tenants/{id}` fans out each present sub-change to the existing owner — `settings` →
`SettingsService.Patch` (FR-TEN-08, JSON-schema validated, embedding.dim immutable), `connection` →
`Lifecycle.Move` (FR-TEN-07, password re-encrypted, C-4), `status` → `Lifecycle.Suspend`/`Resume` (FR-TEN-04) —
resolving the tenant by id first (unknown → `404` before any write; illegal transition / immutable settings →
`409`). `DELETE /admin/tenants/{id}?grace` (default 7 days) calls `Lifecycle.ScheduleDelete` (FR-TEN-05: status →
`deleting`, `delete_after` recorded, `202`); the irreversible teardown after the grace window is the EPIC-09 River
`delete_tenant` job (ADR-0005), so `DELETE` does not enqueue one prematurely. These are the **platform** scope,
deliberately **not** tenant-scoped (a platform admin acts across tenants; FR-ACC-03 governs the `/v1` surface).
Errors speak the SPEC-07 §1 envelope (ADR-0027); `provision.ErrValidation`/`ErrIllegalTransition` were exported
(additive, no behaviour change) so the handlers map failures to 400 vs 409 with `errors.Is`. The `AdminService`
threads a configurable `SSLMode` (from `TENANT_DB_SSLMODE`, mirroring `ragctl enroll --db-ssl-mode`) so a local
non-TLS cluster provisions with `disable` while production defaults to `require`. No schema/migration change (the
`tenants`/`tenant_databases`/`jobs` tables, the `provision_tenant`/`delete_tenant` `job_kind` values and
`delete_after` already existed), so the drift guard stays green; `api/openapi.yaml` regenerated via
`mise run openapi` so the served spec, the drift guard and the contract tests grow with the four routes
(ADR-0028). TDD throughout (service + handler unit tests watched red before the code existed). Unit tests
(`internal/cp/tenants`: create-provisions-records-returns, missing-slug/name-400, provision-error-propagation,
patch status/connection/settings routing, empty-patch-400, unknown-status-400, unknown-tenant-404, delete
schedule-with-grace + unknown-404, list pagination; handler branches via `httptest` incl. envelope shape and
CSRF-mapped statuses) + an e2e golden path over the real control-plane Postgres
(`test/e2e/admin_tenants_e2e_test.go`: provision a **real** tenant DB + role through the mounted router,
CSRF-less-mutation-403 → create-201(+DB row + provision_tenant job) → list → PATCH suspend+settings (DB status +
`tenant.suspend`/`settings.update` audit rows verified) → PATCH resume → PATCH-unknown-404 → DELETE
schedule-with-grace-202(status→deleting, delete_after set) → non-admin-session-403). `TestOpenAPIContractGoldenPath`,
`TestAPIRouterGoldenPath`, `TestSettingsGoldenPath`, `TestJobsGoldenPath`, `TestSourcesGoldenPath`,
`TestTenantLifecycleGoldenPath`, `TestTenantMoveGoldenPath` and `TestTenantIsolationSuite` (SPEC-01 §9, re-run
because `internal/api` changed) all stay green. ADR-0032; ISSUE-0005. _(Pre-existing, unrelated to this story:
`internal/cp/audit/pool.go` and `test/e2e/audit_e2e_test.go` each trip one `revive` lint finding, and
`mise run test` leaks `CONTROL_PLANE_URL`/`PROVISION_DB_URL` into the `internal/cli` "RequireURL" tests (they
pass in a clean env); both pre-date this change and no gated package was touched.)_

## EPIC-05 · Ingestion pipeline — ✅ 42/42 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-05.1 | Document and version store | 5 | ✅ Done | FR-ING-02, ADR-0008, SPEC-03 |
| STORY-05.2 | Go parsers: HTML, Markdown, text, CSV, JSON | 5 | ✅ Done | FR-ING-01, SPEC-05 §2 |
| STORY-05.3 | Parsing sidecar (Python) and Go client | 8 | ✅ Done | FR-ING-11, ADR-0006 |
| STORY-05.4 | Structure-aware chunker | 5 | ✅ Done | FR-ING-03/04, SPEC-05 §3 |
| STORY-05.5 | Embedding provider interface and implementations | 5 | ✅ Done | FR-ING-05, NFR-MNT-02 |
| STORY-05.6 | Sink implementation and commit semantics | 5 | ✅ Done | FR-ING-07, NFR-REL-02, SPEC-05 §5 |
| STORY-05.7 | Job stats and error capture | 2 | ✅ Done | FR-ING-10, SPEC-05 §6 |
| STORY-05.8 | Reindex job with table swap | 5 | ✅ Done | FR-ING-09, SPEC-03 §5, SPEC-05 §7 |
| STORY-05.9 | Garbage collection job | 2 | ✅ Done | SPEC-03 §4 |

**Delivered (STORY-05.1):** `TenantStore.Put` (`internal/documents/put.go`) — the WRITE half of the tenant
document store, the persistence step of the ingestion sink (ADR-0008, SPEC-05 §5). It lives beside the STORY-04.4
read/soft-delete store because it is the **same tenant content** (`documents`/`document_versions`/`chunks`, C-3),
the same tables and the same `*tenant.DB`-only access (ADR-0003, C-1) — not the coverage-gated `internal/ingest`,
whose real inhabitant is the sink *orchestration* at STORY-05.6 (rationale in ADR-0033). `Put(ctx, *tenant.DB,
PutInput)` takes an already parsed, chunked and **embedded** version (SPEC-05 §5 opens no transaction before
embeddings exist, so embeddings are a required input — no unembedded staging; the vector is written via a
pgvector text literal + `$n::vector`, no codec dependency) and: compares the input `content_hash` to the current
version's — **unchanged ⇒ touches only `last_seen_at`** (`Changed=false`, no version, no chunk churn, no embedding
cost); **changed/new ⇒** in ONE `db.Begin` transaction upserts the identity (reactivate + touch), inserts the
immutable `document_versions` row (or reuses a prior version with that exact hash on an A→B→A **rollback** — a
pointer change, ADR-0008), inserts its chunks, then **flips `documents.current_version`** (+`last_seen_at`,
`status='active'`) and commits — so a reader of the `live_chunks` view sees the old version until commit and the
new one **instantly**, never a half-built version and never a non-current version's chunks (SPEC-03 §2 invariants
1–2). No HTTP route/OpenAPI change (a pure data-layer store) and **no migration/schema change** (the tables and
`live_chunks` already exist), so the drift guard stays green. TDD (unit `vectorLiteral`/`validatePut` watched red
first); e2e golden path (`test/e2e/document_store_e2e_test.go` over a real enrolled tenant DB — all four
acceptance bullets plus rollback and the non-current-leak guard); `TestTenantIsolationSuite` (SPEC-01 §9) and
`TestDocumentsGoldenPath` re-run green. Coverage gate green (`internal/tenant` 70.7%; `internal/ingest` not
created ⇒ SKIP). Lint clean on the touched package. ADR-0033; ISSUE-0006. Starts EPIC-05 (5/42).

**Delivered (STORY-05.2):** `internal/ingest/parse` — the native-Go half of the parse stage (SPEC-05 §2,
FR-ING-01). A `Registry` maps a canonical MIME type to a pure, stateless `Parser` and dispatches on the
upload-allowlist MIME (parsers never sniff; charset/params stripped before lookup); `Default()` wires the five
Go formats and deliberately omits the sidecar formats (PDF/DOCX/PPTX/XLSX, STORY-05.3). Every parser emits the
**same `Normalised{Title, Blocks}`** the sidecar returns — a closed block set (`heading`/`paragraph`/`table`/
`list`/`code`) with tables carried as both structured `rows` and GFM markdown — so a Go parser and the sidecar
feed the chunker (STORY-05.4) interchangeably. **HTML** (`golang.org/x/net/html`, no external readability/
markdown dependency): picks the `<main>`/`<article>`/`<body>` content root, skips chrome subtrees (nav/aside/
header/footer/script/form + a class/id/role boilerplate heuristic, ADR-0034), preserves heading levels, renders
inline links/emphasis/code to markdown and tables to GFM, and falls back to the whole `<body>` if chrome removal
empties the doc (extra paragraphs is the documented ceiling — never data loss). **Markdown/text** is a line
scanner (ATX headings, fenced code, pipe tables, lists; text → blank-line-delimited paragraphs). **CSV** → one
markdown table; **JSON** → a table for a uniform array of flat objects (sorted-union columns for determinism),
else a flattened `key.path[i]: value` code block. `Normalised.Markdown()` re-renders the blocks to the
normalised text SPEC-05 §1 hashes. **Fixed a heading-case infinite loop** in the markdown scanner (the ATX-heading
branch never advanced the line cursor, so any `#`-led document — e.g. the changelog fixture — spun forever). TDD
golden-file suite: 11 representative fixtures (>=10 AC) parsed to `testdata/golden/*.json`, plus targeted tests
for boilerplate removal, heading preservation, table→markdown, JSON records/nested, CSV, text paragraphs and
registry dispatch/unsupported-MIME. `go test ./internal/ingest/parse` green; module suite otherwise unchanged
(the pre-existing env-sensitive `internal/cli` `*RequiresURL` failures under mise `.env` injection are the
documented ISSUE-0003 noise). Lint/gofmt clean. ISSUE-0007. EPIC-05 → 10/42.

**Delivered (STORY-05.3):** the Python **parsing sidecar** (`services/parser`) and its **Go client**
(`internal/ingest/sidecar`) — the heavy-format half of the parse stage (ADR-0006, SPEC-05 §2, FR-ING-11).
The STORY-01.2 health-only stub becomes a real Flask/gunicorn service: `POST /parse` (multipart `file`+`mime`)
extracts **PDF** (PyMuPDF — font-size heading heuristic + `find_tables`), **DOCX** (python-docx, body walked in
document order so paragraphs/tables interleave), **PPTX** (python-pptx — slide titles/text/tables) and **XLSX**
(openpyxl — per-sheet heading+table) into the **same `Normalised{title, blocks}`** the Go parsers emit (STORY-05.2),
tables carrying `rows` + a GFM `text` from a `markdown_table` that mirrors the Go `markdownTable` byte-for-byte —
so both producers hash identically and are interchangeable to the chunker. Focused per-format libraries (all
manylinux wheels, non-root `python:3.11-slim`) over a mega-extractor (ADR-0035); the block shape follows SPEC-05
§2 `rows`, reconciling ADR-0006's older `table`. Error taxonomy the worker acts on: **415** unsupported /
**422** parse-failure (both terminal — the worker records `metadata.parse_error` and skips, the sync still
succeeds, SPEC-05 §8) / **429·5xx** transient / **413** too large. The **Go client** `Parse(ctx, filename, mime,
data) → parse.Normalised` applies the 120 s parse budget, retries transport/429/5xx with capped exponential
backoff **honouring `Retry-After`** (cancellable by context), and wraps each call in an **OpenTelemetry span with
W3C trace-context injection** via the installed global propagator — no `otelhttp` dependency. `config.ParserURL`
reads `PARSER_URL` (compose already sets `http://parser:8081`). Six committed fixtures across the four formats
(`gen_fixtures.py`, runtime deps only). `mise run test-parser` (13 pytest, wired into CI, ADR-0014) + `go test
./internal/ingest/sidecar ./internal/config` green (success/decoding, 415/422 terminal-no-retry,
retry-then-succeed, give-up, Retry-After-cut-by-context, trace injection, health); `golangci-lint` 0 issues;
gofmt clean. The image installs every dep as a manylinux wheel (build log confirms) — a clean tagged export
couldn't be confirmed locally (dev docker daemon wedged mid-run), so CI's `integration` job builds it via
compose. No schema/OpenAPI/migration change. ADR-0035; ISSUE-0008. EPIC-05 → 18/42.

**Delivered (STORY-05.4):** the **structure-aware chunker** (`internal/ingest/chunk`) — splits a parsed
document (`parse.Normalised`, from either producer) into retrieval chunks (SPEC-05 §3, FR-ING-03/04).
`Document(n, cfg) []Chunk` walks the blocks maintaining a `heading_path`: prose accumulates to
`TargetTokens` (default 512); a **heading flushes the current chunk and re-roots the path** so every chunk
carries exactly one `heading_path` (AC "respects headings"); **tables and code are atomic** — flushed clear of
prose and never split unless a block alone exceeds 2×target, when it splits on **row** (header repeated) or
**line** boundaries so no row/line breaks; consecutive prose chunks **overlap** by `OverlapTokens` (default 64,
tail words of the previous chunk, counted toward the next budget so the size bound holds). The output `Chunk`
carries **both** `Content` (stored verbatim in `chunks.content`) **and** `EmbedText` — `Content` prefixed with
the SPEC-05 §3 context line `"{title} > {heading path}"` (empty/consecutive-duplicate segments dropped, so a
title equal to H1 doesn't repeat) — the embedder (STORY-05.5) embeds `EmbedText`, the store persists `Content`.
Token counting is an **injectable** `Config.Count` defaulting to a regex approximation (SPEC-05 §3 permits it;
no tiktoken dependency — inject a `cl100k_base` counter later to tighten). `parse.RenderTable` exported (thin
shim) so split table parts re-render. TDD: `go test ./internal/ingest/chunk` green incl. the headline
**property test** (`TestSizeBoundsProperty`, 50 randomised docs: no chunk exceeds target, positions dense) plus
heading_path, context-line-on-embed-only (+ title==H1 dedupe), table/code intact, target/overlap configurable
with overlap carried, oversize table→rows and code→lines. `golangci-lint` 0 issues; gofmt clean; `parse` re-run
green after the export. No schema/OpenAPI/migration change. ADR-0036; ISSUE-0009. EPIC-05 → 23/42.

**Delivered (STORY-05.5):** the **embedding stage** (`internal/ingest/embed`) — turns chunk text into vectors
(SPEC-05 §4, FR-ING-05, NFR-MNT-02). An `Embedder` seam (`Embed(ctx, []string) (Result, error)`) with four
**dependency-free** `net/http`+`encoding/json` providers behind a `registry` (a fifth is one file + one line,
NFR-MNT-02, no vendor SDKs per ADR-0002): **OpenAI** and **Voyage** share the identical
`{model,input}`→`{data:[{index,embedding}],usage.total_tokens}` schema so they are **one** implementation
(`openAICompatible`, data re-sorted by index); **Cohere** (`{texts,input_type:"search_document"}`→`embeddings` +
`meta.billed_units.input_tokens`); **TEI** self-hosted (`{inputs}`→bare `[[...]]`, no usage → `Tokens=0`). The
value `New` returns is **itself an `Embedder`** — a `batcher` that partitions arbitrary input into **≤96-text /
≤100k-token** batches, runs them with **bounded per-tenant concurrency** (`errgroup.SetLimit`, default 4 in
flight — already-present `golang.org/x/sync`, no new dep), guards each batch with a **per-provider circuit
breaker** (closed/open/half-open, `ErrCircuitOpen` short-circuits without hitting the API so the sink can *snooze*
per SPEC-05 §8), and reassembles vectors in **input order** with tokens summed. Per-request **retry/backoff
honouring `Retry-After`** on 429/5xx (context-cancellable, terminal non-2xx not retried) + an **OTel span with W3C
propagation** mirror `internal/ingest/sidecar` exactly (no `otelhttp` dep). **Token usage surfaced** via
`Result{Vectors [][]float32, Tokens int}` — the SPEC-05 §4 signature is *widened* from bare `[][]float32` because
that same section requires token usage be recorded and a bare slice cannot carry it (SPEC-05 §4 updated; ADR-0037);
the sink (STORY-05.6) folds `Result.Tokens` into `usage.Delta.EmbedTokens` (ADR-0024) and `jobs.stats.embed_tokens`
(SPEC-05 §6) — this story only surfaces it. **Provider allowlist fail-closed**: `New` refuses a provider absent
from `settings.providers_allowed` (`ErrProviderNotAllowed`, incl. empty) *before* the registry, so tenant content
never leaves for a non-permitted provider (SPEC-09 §2); `ErrUnknownProvider` for a bad name. TDD (`embed_test.go`
watched fail — no non-test files — before implementation); `go test ./internal/ingest/embed` green and **`-race`
clean**: per-provider request-shape/auth/path + out-of-order reassembly + token surfacing, batching bounds (texts
and token budget, order across batches), bounded concurrency, retry-then-succeed, `Retry-After`-cut-by-context,
terminal-not-retried, breaker opens+short-circuits, allowlist reject + empty-fails-closed, unknown provider, empty
input. `gofmt`/`go vet` clean; pinned `golangci-lint v2.13.1` **0 issues**. No schema/OpenAPI/migration change
(`jobs.stats` is an existing JSON blob, `usage_daily` already exists; this package writes neither). Coverage:
`internal/ingest` reports SKIP against the gate (matches the path exactly, no direct Go files) so the subpackage is
not gated. Module change limited to promoting `golang.org/x/sync` + `go.opentelemetry.io/otel/trace` to direct
(the `golang.org/x/net` promotion is a pre-existing drift fix — `internal/ingest/parse` uses `x/net/html`).
ADR-0037; ISSUE-0010. EPIC-05 → 28/42.

**Delivered (STORY-05.6):** the ingestion **Sink** (`internal/ingest/sink`) — the per-document orchestrator
(SPEC-05 §1/§5, FR-ING-07, NFR-REL-02) a connector (EPIC-06/07) pushes documents at, tying the STORY-05.1–05.5
stages together and owning **no SQL of its own**: every tenant write goes through `documents.Store` and thus a
`*tenant.DB` from the resolver (ADR-0003, C-3). `Sink.Put(ctx, Document)` runs the §1 flow — parse
(`parse.Default()` registry, routing `ErrUnsupportedMIME` to the `sidecar.Client`) → normalise (`Normalised.
Markdown()`) + `sha256` → **hash short-circuit** → `chunk.Document` → `embed.Embedder.Embed` → `documents.Put`
in **one transaction** — stamping every chunk with the configured embedding model (Invariant 3) and its aligned
vector. **Hash-before-embed** is a new atomic store method `documents.TouchIfUnchanged` (one `update … from
document_versions … where content_hash=$hash`): an unchanged document only touches `last_seen_at` (and
reactivates a byte-identical reappearance) and **costs no embedding**, the SPEC-05 §1 short-circuit that
`documents.Put`'s own compare (after embeddings exist) cannot provide. `Sink.Complete` soft-deletes documents
not re-seen this run (`last_seen_at < started_at`) via a second new store method `documents.SoftDeleteUnseen`
**on a FULL sync only** — an incremental sync never deletes; `started_at` is captured at `New`. **Error
classification** (ADR-0038): a single-document parse error or a non-circuit embed error is recorded in
`stats.errors` with `docs_failed++` and the sync continues (§2/§8); `embed.ErrCircuitOpen` returns a
`sink.SnoozeError` so the worker snoozes the job rather than failing it (§8); an infrastructure (store/DB) error
is propagated to fail the job for retry. **Crash safety (NFR-REL-02 teeth):** the per-document transaction is
`documents.Put`'s (ADR-0008) so a worker crash mid-sync leaves **no partial document** — completed documents are
whole and unseen ones are skipped by hash on the retry. The sink accumulates a `Stats` struct whose JSON tags
match the SPEC-05 §6 `jobs.stats` shape (`docs_seen/changed/unchanged/deleted/failed`, `chunks_written`,
`embed_tokens`, `bytes_fetched`, `duration_ms`, `errors:[{external_id,msg}]`); the actual `jobs.stats`/
`usage_daily` persistence and the 100-error cap stay STORY-05.7 (errors left uncapped here, ponytail). Ports
(`LocalParser`/`SidecarParser`/`Store`/`embed.Embedder`) are injected so the orchestration is hermetically
unit-testable; the `*tenant.DB` is an opaque pass-through (nil in unit tests). TDD (`sink_test.go` watched fail
— undefined `New`/`Config`/`Document` — before implementation); `go test ./internal/ingest/sink` green and
**`-race` clean**: changed→embed+store, unchanged→embed skipped, `ErrUnsupportedMIME`→sidecar, parse
failure→`docs_failed`+continue, circuit-open→`*SnoozeError`, non-circuit embed error→recorded, store
error→propagated, full `Complete` soft-deletes / incremental does not, stats+duration accumulation. A DB e2e
(`test/e2e/sink_e2e_test.go`, tag `e2e`, stubbed provider) over a **real enrolled tenant** proves the
per-document transaction (`live_chunks` flip visible on commit), **no partial document** on a mid-transaction
failure (a wrong-dimension chunk rolls the whole version back), crash-retry-by-hash (A unchanged not duplicated,
B changed, C swept), and full-vs-incremental `Complete` — **PASS** (74.5 s). `gofmt`/`go vet` clean; pinned
`golangci-lint v2.13.1` **0 issues** on `internal/ingest/sink` and `internal/documents` (incl. the `forbidigo`
`Unsafe()` ban). No schema/migration/OpenAPI change (the two store methods are new SQL over existing tables;
`jobs.stats`/`usage_daily` are STORY-05.7). Coverage: `internal/ingest/sink` sits under `internal/ingest`
(gate SKIP — matches the path exactly, no direct Go files); `internal/documents` is not gated. ADR-0038;
ISSUE-0011. EPIC-05 → 33/42. _(Environment: `docker compose exec` is ~60 s here, so the e2e reads the tenant id
over the direct control pool rather than the `docker compose exec` psql helper; best-effort `t.Cleanup` teardown
may skip on that slowness.)_

**Delivered (STORY-05.7):** finalises the in-memory ingestion `Stats` value STORY-05.6 left open (SPEC-05 §6,
FR-ING-10) — no worker or `jobs`/`usage_daily` write (that stays STORY-09.1). **100-error cap:**
`Sink.recordFailure` (`internal/ingest/sink/sink.go`) now keeps at most the first `maxDocErrors = 100`
`{external_id, msg}` entries via a single `len(...) < 100` guard in the one error-recording path, while
`DocsFailed` **always** increments — including past the cap — so the count stays honest even when the list is
truncated (retiring the STORY-05.6 ADR-0038 ponytail "errors left uncapped here"). **Shape (SPEC-05 §6):** the
`Stats` JSON tags already matched the jobs.stats shape (`docs_seen/changed/unchanged/deleted/failed`,
`chunks_written`, `embed_tokens`, `bytes_fetched`, `duration_ms`, `errors:[{external_id,msg}]`) with `duration_ms`
filled from the run clock in `Stats()`; `Stats()` now also normalises `Errors` to a non-nil slice so a clean run
marshals `errors` as an empty list (`[]`), never `null`. TDD (`sink_test.go`): `TestErrorsCappedAtHundred`
(150 failing docs ⇒ `len(errors)==100` but `docs_failed==150`, first 100 kept) and
`TestStatsEmptyErrorsMarshalAsList` (`errors:null` pre-fix) watched red for the right reasons, then green;
`TestStatsMarshalMatchesSpecShape` asserts the marshalled object carries exactly the 10 SPEC-05 §6 keys and that
`errors` is a list of objects with exactly `external_id` and `msg`. `gofmt -l`/`go vet` clean;
`go test ./internal/ingest/sink` green and `-race` clean; pinned `golangci-lint v2.13.1` **0 issues**. No new ADR
(the cap + list-shape realise the SPEC-05 §6 contract and the ADR-0038 upgrade path, not a new decision); no
schema/migration/OpenAPI change. Coverage: `internal/ingest/sink` under `internal/ingest` (gate SKIP). ISSUE-0012.
EPIC-05 → 35/42.

**Delivered (STORY-05.8):** the **resumable reindex operation** — migrate a tenant's live chunks to a new
embedding model and/or dimension while retrieval keeps serving from the current `chunks` table, with an atomic
swap and the old table dropped only after a coverage verification (FR-ING-09, SPEC-05 §7, SPEC-03 §5). Two pieces,
both reached ONLY through `*tenant.DB` (ADR-0003, C-1, C-3), no raw pool (`Unsafe()` ban intact): the tenant-side
DDL/DML on `documents.TenantStore` (`internal/documents/reindex.go`: `CreateChunksNew`, `LiveVersionsAfter`,
`VersionChunks`, `InsertChunksNew`, `VerifyCoverage`, `SwapChunks`, `DropChunksOld` — kept OFF the `documents.Store`
interface, so the documents Service/handlers keep a minimal surface; the reindex declares its own narrow port
satisfied structurally, the ADR-0038 sink precedent) and the orchestration `internal/ingest/reindex` (owns no SQL;
composes the store + chunker + `Embedder`). **Off the hot path:** `Prepare` builds an empty `chunks_new` at the new
`vector(N)` (`LIKE chunks` + `ALTER … TYPE vector(N)` on the empty column + explicit PK/unique/**cascade FKs**/
btree/GIN/HNSW; the dimension is an interpolated positive-int since a typmod cannot bind, exactly as the migration
runner substitutes `EMBEDDING_DIM`), and `live_chunks` keeps reading the old `chunks` — **queries are undisturbed
during the build**. **Resumable:** `Step(cursor)` re-embeds one batch of live versions (active docs' `current_version`)
after a `document_id` cursor in id order and returns the advanced cursor the worker persists; `InsertChunksNew` is
**atomic + idempotent per version** (delete-then-insert in one tx), so a crash/retry **resumes rather than restarts
or duplicates**. **Verify-before-swap/drop:** `VerifyCoverage(table)` counts live versions represented in a target
table; `Swap` refuses (`ErrNotVerified`) unless `chunks_new` covers every live version, and `DropOld` re-checks the
now-live `chunks` table before dropping `chunks_old` — a durable-state check that holds across a crash between swap
and drop. **Atomic swap:** `SwapChunks` runs SPEC-03 §5 in ONE DDL transaction (drop `live_chunks`, rename
`chunks`→`chunks_old` and `chunks_new`→`chunks`, recreate `live_chunks`), so no query sees a half-swapped state.
**Dimension change vs ADR-0022:** the reindex is the *sanctioned* path the `embedding_dim` immutability points at —
the physical `vector(N)` moves in the swap; the driving worker (STORY-09.1) moves the control-plane
`settings.embedding_dim` mirror and configured model as the job's finalize step, so the mirror never desyncs from the
live column (ADR-0039). Re-chunk (settings changed) re-parses the stored normalised markdown; the default
model/dimension reindex re-embeds the existing chunk text verbatim (exported `chunk.WithContext` reconstructs the
SPEC-05 §3 embed context line). **TDD**: `reindex_test.go` over a fake store + fake embedder — the verify-before-swap
and verify-before-drop gates watched **red** (guard disabled ⇒ both refusal tests fail) then **green**; plus
Step batching/cursor, resume-not-restart, reuse embed-text reconstruction, re-chunk branch, preconditions. **e2e**
(`test/e2e/reindex_e2e_test.go`, real enrolled tenant, dim 768 → 384): live_chunks keeps serving the old `vector(768)`
table during a partial build, a **refused swap** at 1/3 covered, **resume-from-cursor** completing the remaining 2
docs, the **atomic swap** flipping `live_chunks` to `vector(384)` with the new model, and `chunks_old` dropped only
after post-swap verification — **green in ~76 s**. `gofmt -l`/`go vet` clean; `go test ./internal/ingest/...
./internal/documents/...` green; pinned `golangci-lint v2.13.1` **0 issues**. No schema/migration/OpenAPI change
(`chunks_new`/`chunks_old` are transient; drift guard green). `TestTenantIsolationSuite` could not run here — its
every assertion goes through `docker compose exec`, which is currently wedged in this environment (a bare
`docker compose exec postgres psql -c 'select 1'` did not return within 120 s) while the DB is healthy; the
`Unsafe()` ban it protects is lint-enforced (green) and the per-tenant role's create/swap/drop is proven by the
reindex e2e over the direct pool. ADR-0039; ISSUE-0013. EPIC-05 → 40/42.

**Delivered (STORY-05.9):** the **retention garbage-collection sweep** — `documents.TenantStore.CollectGarbage`
(`internal/documents/gc.go`), the operation the SPEC-08 `gc_tenant` daily job (STORY-09.1, out of scope) drives
per tenant (SPEC-03 §4). All four SPEC-03 §4 classes are collected through ONE resolver `*tenant.DB` (ADR-0003,
C-1, C-3), no raw pool (`Unsafe()` ban intact): **non-current `document_versions`** older than the window (chunks
removed by the `version_id` cascade; a *current* version is never a victim, so invariant 2.1 holds),
**`status='deleted'` documents** past grace (their versions + chunks cascade on `document_id`), **`query_log`**
past retention (feedback cascades), and **stale `crawl_pages`**. Windows come from a `GCPolicy` whose zero fields
fall back to the SPEC day-defaults (30/30/90 d); `BatchSize` (default 1000) **bounds every delete** to a
keyset-limited CTE so a huge backlog is drained in many small transactions instead of one table-locking delete,
and the sweep is **idempotent** (a second run over the same `now` removes nothing). `GCMetrics` returns
**rows removed per class** (plus cascaded `Chunks`, informational) for the worker to emit (SPEC-10). **Crawl-page
caveat (ponytail, kept OFF the `documents.Store` interface, ISP/ADR-0038):** SPEC-03 §4 phrases the rule as "not
seen in 3 successful syncs" — a generation count the tenant schema does not record (no per-source sync counter;
the crawler is EPIC-06/07). GC approximates it as "not fetched within `CrawlPageStale`" (the worker supplies
3×cadence); a **zero window skips** the crawl sweep rather than inventing a threshold, and null-`last_fetched_at`
pending pages are left alone. Upgrade path (noted in `gc.go`): add `crawl_pages.last_seen_sync` + a per-source
counter when the crawler lands and switch to a generation delta. **TDD**: `gc_test.go` pins the pure policy/metrics
logic (defaults incl. negative-batch clamp and crawl-skip; `Total` excludes chunks) — watched **red** (undefined
symbols) then **green**. **e2e** (`test/e2e/gc_e2e_test.go`, real enrolled tenant at dim 4): seeds each class with a
collectible *and* a retained row plus live data, then proves each collectible row **and its cascade** is gone, the
current version / live_chunks / within-grace doc / recent log / fresh+pending crawl pages survive, the per-class
counts are exact (`{2,1,2,1, chunks 4}`), and a second run is all-zero — **green in ~9 s** (`BatchSize=1` exercises
the drain loop). `gofmt -l`/`go vet` clean; `go test ./internal/ingest/... ./internal/documents/...` green; pinned
`golangci-lint v2.13.1` **0 issues** on `internal/documents/...`. No schema/migration/OpenAPI change — GC reuses
existing `created_at`/`deleted_at`/`last_fetched_at` columns and the schema's FK cascades, so the drift guard stays
green (no ADR needed; references SPEC-03 §4, ADR-0008/0017). ISSUE-0014. **EPIC-05 → 42/42 ✅.**

## EPIC-06 · Connector framework and upload connector — ✅ 13/13 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-06.1 | Connector interface, registry, config validation | 5 | ✅ Done | FR-SRC-13, FR-SRC-14, NFR-MNT-01, SPEC-04 §1, ADR-0040 |
| STORY-06.2 | Credential encryption and handling | 3 | ✅ Done | FR-SRC-10, SPEC-04 §6, SPEC-09 §2, ADR-0041 |
| STORY-06.3 | Upload connector and ingest_document job | 5 | ✅ Done | FR-SRC-02, SPEC-04 §5, SPEC-05, ADR-0042 |

**Delivered (STORY-06.1):** the connector framework — a new `internal/connector`
package (FR-SRC-13, FR-SRC-14, NFR-MNT-01, SPEC-04 §1/§7, ADR-0040) plus the wiring
of its config validation / "test connection" into the sources API `Validator` seam
STORY-04.3 left nil (ADR-0029). The full SPEC-04 §1 `Connector` interface
(`Kind`/`ValidateConfig`/`Test`/`Sync`) and its supporting types (`Credentials`,
`Document`, `Sink`, `SyncRun`, `StateStore`, `Stats`) are transcribed faithfully so
EPIC-07 connectors have a frozen target (building the interface is the named
deliverable, and NFR-MNT-01 wants the contract stable — not scope creep, since no
connector is implemented); `StateStore`/`Stats` are minimal and documented
provisional because only `Sync` (EPIC-07) exercises them (the canonical jobs.stats
shape stays SPEC-05 §6). A `Registry` maps a kind to a `func() Connector` factory
(fresh instance per `Lookup`, since a connector may hold per-sync state; duplicate/
nil registration panics — init-time misconfiguration fails loudly), with a
process-wide `DefaultRegistry()` behind package-level `Register`/`Lookup` connectors
use from `init()` (SPEC-04 §1). Config validation is a `SchemaValidator` +
`ConfigError`/`FieldError` reusing the STORY-03.5/ADR-0022 JSON-Schema pattern over
`santhosh-tekuri/jsonschema/v6` (one validation approach, no new validation dep);
connectors embed a schema and call it from `ValidateConfig` (SPEC-04 §7 step 2). The
seam is bridged by a `SourcesValidator` adapter that satisfies the sources package's
local `Validator` interface *structurally* — the sources package keeps no connector
import (ADR-0029 preserved); the dependency is injected in `internal/cli`
(`connector.NewSourcesValidator(connector.DefaultRegistry(), sources.ErrConnectorUnavailable)`).
For an unregistered kind `ValidateConfig` returns nil (kind-specific validation
deferred; generic validation still applies, so a source whose connector is not built
yet can still be created) and `Test` returns the injected
`sources.ErrConnectorUnavailable` sentinel (so `/test` keeps the not_found seam) —
never a 500 or a fake 200 (AGENTS.md Integrity). No connector is registered in v1
yet (upload is STORY-06.3, crawl/api/sitemap EPIC-07), so wiring a real connector is
its package + one `Register` call — no change to `internal/connector`, the sources
package, or the router (NFR-MNT-01). Credentials are still not threaded into `Test`
(STORY-06.2) and no credential reaches this package (C-4); sources stay control-plane
registry data and this package touches no database (C-3, ADR-0003). New direct
dependency `golang.org/x/time` v0.3.0 for `SyncRun.Limiter *rate.Limiter` (SPEC-04
§1). No schema/migration change and no new HTTP route (the `/test` route already
existed), so `schemas/*.sql`, the drift guard and `api/openapi.yaml` are unchanged.
TDD throughout (schema/registry/validator unit tests watched red before the package
existed); `go test -cover ./internal/connector/` = **85.4%** (gate 70%). e2e
(`test/e2e/connector_e2e_test.go`) over the real control-plane Postgres and the real
`internal/api` router registers a fake `web_crawl` connector into a fresh registry,
wires the real `SourcesValidator`, and proves through the API-key admin chain:
invalid connector config → 400, valid → 201 (persisted), `/test` → 200 with the
connector's `Test` called once, and an unregistered kind → 201 (deferred) with
`/test` → 404 seam. ADR-0040; ISSUE-0015. _(Pre-existing, unrelated to this story:
`docker compose exec` is wedged in this environment, so the `psql`-asserting
`TestSourcesGoldenPath`/`TestTenantIsolationSuite` cannot complete here — ISSUE-0014;
this story touched none of `internal/tenant`/`internal/api`/`internal/worker`.
`golangci-lint` v2.13.1 needs Go ≥ 1.26 vs the local 1.22, so `mise run lint` uses
its `go vet` offline fallback (clean); `internal/cli` unit tests pass with a clean
environment and fail only under mise's leaked `.env`.)_

**Delivered (STORY-06.2):** source credential encryption and handling (FR-SRC-10,
SPEC-04 §6, SPEC-09 §2, ADR-0041) — credentials sealed on write, never returned,
decrypted only for a Test/Sync and zeroed after, with sanitised errors. It replaces
the STORY-04.3 fail-closed `400` credentials stub with real encrypt-on-write and
threads decrypted credentials into the connector `Test` seam STORY-06.1 left passing
`nil`. **Reuse over new (C-4):** the same platform envelope `crypto.Cipher` the
resolver/provisioner use (AES-256-GCM DEK wrapped by KMS, SPEC-09 §2) seals the
credentials — no new crypto scheme or dependency — so `ragctl keys rotate-dek` covers
`sources.credentials_enc` for free; the column already existed, so **no migration**.
The sources `Service` gains injected `Encrypter`/`Decrypter` ports (`*crypto.Cipher`,
wired in `internal/cli` from the startup cipher). **Write path:** the public
`credentials` body is a flat `map[string]string` (a non-string/nested value fails to
decode → `400`, shape-validated for free); the service marshals → encrypts → zeroes
the plaintext JSON buffer → nils the plaintext map → hands the store only the
ciphertext (`CredentialsEnc []byte`), so the store never sees plaintext, and a missing
Encrypter with credentials present fails closed (never a plaintext store, C-4).
**Never returned, structurally:** the public `Source` projection and `sourceColumns`
keep omitting `credentials_enc`; a dedicated `Store.GetCredentials` is the *only* read
of the column, used solely by the decrypt path. **Decrypt-and-zero:** `Test` reads the
ciphertext, decrypts into a map, zeroes the decrypted `[]byte` immediately after
unmarshalling, passes the map through the widened `Validator.Test(…, creds)` seam
(`connector.SourcesValidator` forwards it as `connector.Credentials`), and `clear()`s
the map the moment `Test` returns (`defer`) — the same helper feeds the future sync
worker's `SyncRun.Creds` (EPIC-07/09); `Sync` itself is **not** implemented here (task
scope). A new `crypto.Zero([]byte)` does the buffer wipe. _ponytail:_ Go strings (the
map values) cannot be overwritten in place, so "zeroed" means the decrypted buffer is
wiped and the map cleared (values GC-eligible); a `[]byte`-valued credential type is
the upgrade path (ADR-0041). **Sanitised errors:** crypto/decrypt errors carry neither
the ciphertext nor a secret value, a connector `Test` failure maps to the generic
public envelope (never the raw error), and credentials are never logged. No OpenAPI
change (the generator does not model request-body properties, so the drift guard stays
green — mirroring STORY-06.1). TDD throughout (tests watched red before implementation:
`crypto.Zero`; sources encrypt-on-write / fail-closed / decrypt-and-zero /
zeroed-after-use / sanitised-error; connector creds-forwarding; handler seal +
non-string rejection). `go test -cover ./internal/connector/` = **85.4%** (gate 70%);
the sources store SQL for `credentials_enc` is e2e-covered (sources is not gated). e2e
(`test/e2e/credentials_e2e_test.go`) over the real control-plane Postgres and the
**real** Cipher registers a credentials-recording connector and proves through the
API-key admin chain: create-with-credentials → 201 with no secret echoed; the stored
`credentials_enc` is ciphertext (asserted != plaintext, round-trips via the cipher);
GET returns no credentials; `/test` decrypts and hands the plaintext to the connector.
`TestSourcesGoldenPath` (now wires the real Cipher, asserts credentials accepted +
sealed + not echoed) and `TestConnectorFrameworkGoldenPath` stay green. ADR-0041;
ISSUE-0016. _(Pre-existing, unrelated to this story: `docker compose exec` is wedged
here — ISSUE-0014 — so the `psql`-asserting tail of `TestSourcesGoldenPath` cannot
complete locally, though its credential assertions pass; `internal/cli` unit tests and
`mise run coverage`/full `lint` remain red only for the documented mise `.env`-leak and
golangci-lint/go1.26 toolchain drift on untouched files — verified this story added no
new lint finding and touched none of `internal/tenant`/`internal/api`/`internal/worker`.)_

**Delivered (STORY-06.3):** the upload connector and the `ingest_document` job handler
(FR-SRC-02, SPEC-04 §5/§5a, SPEC-05, SPEC-07 §2, ADR-0042; ISSUE-0017) — **this
completes EPIC-06 (13/13)**. The story fills the three STORY-04.4 upload seams and adds
the handler that turns a queued job into a document. **(1) Object storage:** a new
`internal/objectstore` S3-compatible client (`aws-sdk-go-v2/service/s3`, reusing the
already-vendored SDK core — no `minio-go`; one client path for local MinIO and prod),
path-style + custom endpoint, bucket-ensure, `Put`/`Get`, fail-closed on missing config;
wired behind the documents `Storage` seam in `internal/cli` (an unset/unreachable store
at boot logs a warning and leaves uploads on the not_found seam — reads keep working).
**(2) MIME sniffing + size from settings:** the handler now sniffs the file's leading
bytes (`http.DetectContentType`) and requires them to match the extension allowlist —
the client `Content-Type` is never trusted, so a mislabelled/hostile file (an executable
as `.txt`, a non-PDF `.pdf`, a non-zip `.docx`) is rejected `400` before storage; the
size ceiling is the per-tenant `settings.limits.max_upload_mb` (SPEC-02 §5) read via a
new `UploadLimits` port, failing safe to the global `MAX_UPLOAD_BYTES`. **(3) Implicit
upload source:** a new `UploadSource` port resolves (idempotently upserts) the tenant's
`upload` source on the control-plane pool (C-3) so an upload with no explicit `?source`
still has a `source_id` — an ordinary `sources` row (kind `upload`, `(tenant_id, name)`
unique), **no schema change**. **(4) Upload connector:** `internal/connector/upload`
registers the `upload` kind (so the sources `/test` and config-validation seams resolve
it), with `ValidateConfig` accepting any object, a no-op `Test`, and a `Sync` that fails
loudly (`ErrNotScheduled`) — upload is not scheduled (SPEC-04 §5); blank-imported at the
composition root (NFR-MNT-01). **(5) `ingest_document` handler:** a new
`internal/ingest/ingestdoc` package — a directly-callable `Ingestor.Dispatch`/`Run` (the
River worker that dispatches to it is EPIC-09 STORY-09.1, deliberately NOT built) that
fetches the bytes, loads tenant settings, builds the Embedder (an injected
`EmbedderFactory` seam — the production factory with provider keys is EPIC-09; tests
inject a deterministic stub, as the provider is external, matching the sink e2e), and
runs one INCREMENTAL `Sink.Put` (parse→chunk→embed→commit) so ingesting one upload never
soft-deletes the tenant's other documents. **Doc-row-creation boundary (the SPEC
tension), resolved:** SPEC-04 §5 prose says the upload path "creates a document row," but
an active document must have a non-null `current_version` and there is no pending status
(SPEC-03 §2 invariant 1, ADR-0008); the invariant wins (README-vs-ADR convention) — the
HTTP handler creates **no row**, and the `ingest_document` handler builds the row **and**
its first version together in the one `TenantStore.Put` transaction, so `live_chunks`
never shows a half-built document. A re-upload of the same filename (identity `(upload
source, filename)`) hashes to a new immutable version and flips `current_version`
(STORY-05.1). No OpenAPI change (the route/response already existed, STORY-04.4), so the
served spec and its drift guard stay green; no migration, so the schema drift guard stays
green. TDD throughout (tests watched red before implementation): `sniffUpload`
match/mismatch/unknown-ext; service implicit-source resolution + per-tenant limit; handler
byte-sniff rejection + per-tenant oversize; the upload connector kind/validate/test/sync/
registration; objectstore fail-closed; `ingestdoc` `parseSettings`/`JobFromPayload`/`Run`;
`SettingsUploadLimits` extraction. e2e (`test/e2e/upload_ingest_e2e_test.go`) against the
real stack (real MinIO, real control-plane Postgres, a real enrolled tenant DB; embedding
provider stubbed): the bytes land in MinIO (read back via the S3 client), a real
`ingest_document` job is enqueued, `Dispatch` creates the document + first version +
chunks, and a re-upload creates a second version and flips `current_version` — asserted
via the pgxpool / S3 client directly, never `docker compose exec` (ISSUE-0014). ADR-0042;
ISSUE-0017. _(Pre-existing, unrelated: `internal/cli` unit tests fail only under mise's
`.env` injection — `CONTROL_PLANE_URL`/age-key leak — and pass with a clean env;
`docker compose`/container creation is wedged here (ISSUE-0014), so the local MinIO host
port had to be published out of band to run the e2e; no gated package's behaviour
regressed and no new lint finding was introduced.)_

## EPIC-07 · Web crawl, sitemap and API connectors — ✅ 39/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-07.1 | Web crawler core | 8 | ✅ Done | FR-SRC-03/04, SPEC-04 §2, ADR-0043 |
| STORY-07.2 | SSRF protection and egress rules | 3 | ✅ Done | NFR-SEC-04, SPEC-09 §4, ADR-0044 |
| STORY-07.3 | HTML content extraction quality | 5 | ✅ Done | FR-SRC-05, SPEC-04 §2b, ADR-0045 |
| STORY-07.4 | Conditional fetch and change detection | 3 | ✅ Done | FR-ING-02, SPEC-04 §2c, ADR-0046 |
| STORY-07.5 | Sitemap connector | 3 | ✅ Done | FR-SRC-06, SPEC-04 §3/§3a, ADR-0047 |
| STORY-07.6 | HTTP API connector: auth and pagination | 8 | ✅ Done | FR-SRC-07, SPEC-04 §4/§4a, ADR-0048 |
| STORY-07.7 | HTTP API connector: templating and incremental sync | 5 | ✅ Done | FR-SRC-07/08, SPEC-04 §4/§4b, ADR-0049 |
| STORY-07.8 | Source "test connection" for all kinds | 2 | ✅ Done | FR-SRC-14, NFR-SEC-04, SPEC-04 §1b, ADR-0050 |
| STORY-07.9 | Connector documentation | 2 | ✅ Done | SPEC-04, ISSUE-0027 |

**Delivered (STORY-07.1):** the `web_crawl` connector and crawl core
(`internal/connector/webcrawl`, ADR-0043, ISSUE-0018) — the first real
`Connector.Sync`. Level-synchronous BFS with `max_depth`/`max_pages` limits (the
cap is an atomic pre-check that actually stops the crawl), allow(prefix)/deny(substring)
frontier gating, bounded concurrency, robots.txt honoured per host, per-host delay +
`SyncRun.Limiter`, hand-rolled URL normalisation and `<link rel=canonical>`→ExternalID
de-dup (no `purell`/`temoto` dependency). Crawl state persists to `crawl_pages` via a
`CrawlState` capability on `SyncRun.State` (`NewTenantPageStore`, a tenant.DB adapter,
ADR-0003) so an interrupted crawl **resumes** — proven by an e2e over the real tenant
DB. Egress (`Doer`, 07.2), extraction (raw `Body`, 07.3) and conditional-fetch state
(etag/last-modified/hash persisted, 07.4) are left as clean seams. No migration, no
OpenAPI change; coverage 78.2%.

**Delivered (STORY-07.2):** the SSRF egress guard (`internal/egress`, ADR-0044,
ISSUE-0019) closing the STORY-07.1 `Doer` seam (NFR-SEC-04, SPEC-09 §4). Enforcement
is a `net.Dialer.Control` hook that validates the concrete resolved IP at connect —
so DNS rebinding/TOCTOU is structurally closed and every redirect hop re-validates for
free. `IsBlocked` refuses loopback, RFC1918, IPv6 ULA `fc00::/7`, link-local (incl.
the `169.254.169.254` metadata IP), multicast, broadcast, unspecified and IPv4-mapped
private addresses over stdlib `net.IP` (no dependency), allowing only global unicast.
The guard is the connector's **fail-closed default** (`defaultDoer` → `egress.GuardedClient`;
tests inject a permissive Doer via `SetEgressDoerForTest`), so production is safe with
no `internal/cli` change and the sitemap (07.5)/API (07.6) connectors can reuse the
package. The 20 MB response cap is enforced by rejection in the crawler read path; the
30 s timeout is on the client. A table test covers **each** blocked class; a
redirect-to-metadata test proves per-hop blocking. No migration, no OpenAPI change;
coverage 82.1% (egress) / 79.9% (webcrawl).

**Delivered (STORY-07.3):** quality HTML content extraction behind the STORY-07.1
parse seam (`internal/connector/webcrawl/content.go`, ADR-0045, ISSUE-0020,
FR-SRC-05). Per-source `include_selectors`/`exclude_selectors` (CSS, compiled with
`github.com/andybalholm/cascadia` v1.3.2 — the sole new dependency, needing only the
vendored `x/net`, no toolchain bump) prune the DOM; with no usable include match the
whole (exclude-applied) document falls back to the platform's existing semantic
extractor `parse.htmlParser` (ADR-0034) for readability + markdown — one extraction
engine, not a forked one. HTML is now emitted as `Document.Text` markdown
(`MimeType` `text/markdown`); non-HTML still passes as raw `Body`. Title precedence
`<title>`→`og:title`→`<h1>`. `go-readability`/`html-to-markdown` were deliberately
NOT added (ADR-0034 + the `go 1.22` pin; see ADR-0045). Acceptance is a 20-page
synthetic-but-representative golden corpus (`testdata/corpus/`, committed `expected/
*.md` baselines) with a reproducible boilerplate-removal metric guarded by a
content-retention == 1.0 check: **mean removal 1.00** (threshold 0.90), retention
1.00, hermetic. No schema, no migration, no OpenAPI change; webcrawl coverage 81.5%.
Corpus caveat: synthetic, pending a one-time human spot-review (ADR-0045).

**Delivered (STORY-07.4):** conditional fetch and change detection closing the
STORY-07.1 seam (`internal/connector/webcrawl/crawl.go`, ADR-0046, ISSUE-0021,
FR-ING-02). When `crawl_pages` holds a prior ETag/Last-Modified the fetch sends
`If-None-Match`/`If-Modified-Since`; a **304 Not Modified** re-sees the page with NO
read/parse/extract/emit — only `last_fetched_at` is bumped (validators kept). For the
common no-validator case, a 200's raw-body `sha256` is compared to the stored hash and
an identical page is not re-emitted; only changed bytes re-emit (and their new links
re-enter the frontier). New validators are stored on every 200 so the next crawl is
conditional. Conditional GET → 304 is used over HEAD (one round trip, no body when
unchanged; ADR-0046). An **incremental** sync (`Full == false`) re-visits fetched
pages conditionally; a **full** sync keeps the 07.1 resume-skip. Deletion-detection
reconciliation: conditional skip runs only on incremental syncs, where the sink's
`Complete` is a no-op (SPEC-05 §5), so an unchanged, un-emitted page can never be
soft-deleted (it is still marked seen in `crawl_pages`); the full-sync cheap-304
re-see for deletion detection needs an EPIC-09 sink "mark seen" signal, until then
deletion is the periodic full re-enumeration (SPEC-04 §4). TDD: three RED unit tests
(304-no-parse/no-emit with a trap-link proof, no-ETag same-hash no-emit,
changed-bytes re-emit) + a real-Postgres e2e (`TestWebCrawlConditionalFetch`: full
crawl stores ETag+hash, incremental crawl → 304 → empty sink, `last_fetched_at`
advanced, validators intact); the 07.1 resume e2e still passes. No schema, no
migration, no OpenAPI change; drift guard green; `go vet` clean.

**Delivered (STORY-07.5):** the `sitemap` connector (`internal/connector/webcrawl/
sitemap.go` + `sitemapconn.go`, ADR-0047, ISSUE-0022, FR-SRC-06). It drives the SAME
crawl core as `web_crawl` — the central requirement — via two behaviour-preserving
seams in `crawl.go`: a `crawler.seeds` **frontier source** (nil ⇒ derive from
`start_urls` as before; non-nil ⇒ the sitemap's URLs) and a **`followLinks`** flag
(default true; the sitemap connector clears it and pins `max_depth=0`, so the frontier
is exactly the sitemap's URLs and no on-page link is enqueued). Everything else —
SSRF-guarded egress (07.2), conditional GET/304/content-hash (07.4), HTML→markdown
extraction with selectors + readability (07.3), canonical→`ExternalID` de-dup, robots,
per-host delay, size/timeout caps, and the `crawl_pages` `PageStore` over `tenant.DB`
(ADR-0003) — is shared code, not a fork. `<urlset>`/`<sitemapindex>` are parsed with
stdlib `encoding/xml` (recursive index expansion); gzipped `.xml.gz` sitemaps are
inflated with stdlib `compress/gzip` by magic-byte detection — **no new dependency**.
The sitemap tree is bounded (`maxSitemapDepth`/`maxSitemapDocs`/`maxSitemapURLs`, a
`ponytail:` ceiling) against a hostile tree, and sitemap fetches reuse the egress
`Doer` + 20 MB cap. `<lastmod>` incremental: on an incremental sync a URL whose sitemap
`lastmod` is not newer than its recorded `crawl_pages.last_fetched_at` is skipped with
**no request** (cheaper than the 07.4 conditional GET); the two layers compose. Lives
in the webcrawl package (unexported-core reuse), so its `init()` registers `sitemap`
via the composition root's existing blank import — no `internal/cli` change. TDD:
sitemap-index/child, gzip, no-link-follow and lastmod-skip unit tests (RED first) +
a real-Postgres e2e (`TestSitemapSyncAndLastmodIncremental`: index→child→pages full
sync emits all/records `crawl_pages`/follows no link; incremental lastmod-older sync
re-fetches and emits nothing); the 07.1/07.4 web_crawl e2e still pass. No schema, no
migration, no OpenAPI change; drift guard green; `go vet` clean; webcrawl coverage
79.3%. Remaining EPIC-07 stories (07.7–07.9) are Todo.

**Delivered (STORY-07.6):** the HTTP API connector engine (`internal/connector/api`,
ADR-0048, ISSUE-0023, FR-SRC-07) — the third real `Connector.Sync`. **Auth (4 types)**:
`buildAuthedClient` composes the SSRF-guarded egress client with the source's auth shape
and the decrypted `connector.Credentials` (secrets NEVER from config, C-4/ADR-0041,
fails closed, errors name only the missing key): api_key_header, bearer, basic, and
`oauth2_cc` **reusing `golang.org/x/oauth2/clientcredentials`** (no hand-rolled refresh,
**no new dependency** — a sub-package of the already-required `x/oauth2`). The guarded
`*http.Client` is threaded into the oauth2 ctx (`oauth2.HTTPClient`), so the **token
endpoint AND every API call are SSRF-guarded** (ADR-0044); a test with the real guard
proves a loopback `token_url` is blocked. **Pagination (5 types)** — none/page/offset/
cursor/link-header — each with a `max_pages` ceiling (`ponytail:`) so a broken API that
never signals the end still terminates; `SyncRun.Limiter` politeness + `429`/`503`
`Retry-After` (delta-seconds or HTTP-date) bounded retry; a 20 MB response cap and
query-redacted error/log URLs. **JSON paths** are a hand-rolled dot-path evaluator over
`encoding/json` (`$.data`/`$.next_cursor`/`$.category.name`, `UseNumber` for exact
numeric cursors) — no dependency, reused by 07.7. The **07.6/07.7 seam** is the single
`buildDocument` (a placeholder raw-JSON document now; 07.7 adds template/uri/metadata
mapping and incremental sync), and the config schema already accepts the 07.7 fields.
One blank import at the composition root registers `api` (NFR-MNT-01); `source_kind`
already has `api` — no migration, no OpenAPI change. TDD: jsonpath/auth/paginate unit
tests RED first, then the **4×5 auth×pagination matrix** through the real `Sync`, the
oauth2 cache-then-refresh test, the SSRF token-endpoint block, and the 429+`Retry-After`
retry — all httptest/in-process (hermetic; the story touches no DB). `go vet` clean,
`gofmt` clean, drift guard green; api-package coverage 80.2%. Remaining EPIC-07 stories
(07.7–07.9) are Todo.

**Delivered (STORY-07.7):** the HTTP API connector's per-item MAPPING + incremental sync
(`internal/connector/api`, ADR-0049, ISSUE-0024, FR-SRC-07/08), filling the 07.6
`buildDocument` seam. **Templates** (`mapping.go`): `template` and `uri_template` are Go
`text/template` parsed ONCE per endpoint (`docMapper`) and executed per item with the
item's decoded JSON as the dot context → `Document.Text` (`text/markdown`) and
`Document.URI`; helper `FuncMap` `join`/`money`/`date` are total (never error, predictable
fallbacks). Missing fields render `<no value>` (the `text/template` default; `missingkey`
left at `invalid` — `zero` gives `<nil>` for a `map[string]any`, `error` would drop a whole
doc). A template PARSE error is a config error (caught in `ValidateConfig`/`Test`); an
EXECUTION error records-and-skips ONE item (never aborts the sync) and carries only the
author path + Go types, no item content. **Metadata** reuses the 07.6 dot-path evaluator
(no JSONPath dep) → `Document.Metadata`; `id_path`→`ExternalID`, `updated_path`→
`ModifiedAt`. **Incremental** (`incremental_param` + cursor in `State`): a non-full run
with a stored cursor sends `updated_since=<cursor>` on every page (baked onto the endpoint
Path — paginate.go untouched), tracks the max `updated_at`, and persists the VERBATIM
source value back so the next run resumes in the API's own format; a first run / full run
sends none. `SyncRun.Full` reconciles the mode (full ⇒ full enumeration + `sink.Complete`
deletion detection; incremental ⇒ `Complete` no-op, SPEC-05 §5); the cursor advances on
both. **State backing**: a NEW generic tenant table `connector_state` (migration
00002_connector_state.sql; schema version → 2) via `tenantStateStore`/`NewTenantStateStore`
over `*tenant.DB` (ADR-0003, C-3; no `tenant_id`, no cross-DB FK) — the generic per-source
KV the SPEC-04 §1 `StateStore` promised, distinct from the crawler's `crawl_pages`. The
**weekly full sync** cadence (setting `SyncRun.Full` + a full-mode sink) is the EPIC-09
scheduler's job; the connector supports both modes and records `api:last_full_sync` as a
breadcrumb, without self-promoting (the sink mode is the worker's — ADR-0049, mirroring
ADR-0046). TDD: `mapping_test.go` + `incremental_test.go` RED first, then GREEN (template
golden mapping, each helper, missing-field, parse/execution errors, cursor round trip,
full-vs-incremental, nil-State) with an in-memory `StateStore`; the golden-path e2e
(`test/e2e/api_e2e_test.go`) proves the cursor persists to and reloads from
`connector_state` in a REAL enrolled tenant DB across two runs (PASS). Lint clean
(golangci-lint v2.13.1, 0 issues), tenant drift + version guards green; api-package
coverage 79.3%. No control-plane/OpenAPI change, no new dependency. Remaining EPIC-07
stories (07.8–07.9) are Todo.

**Delivered (STORY-07.8):** live "test connection" for all connector kinds (ADR-0050,
ISSUE-0025, FR-SRC-14) — each `Connector.Test` now probes reachability AND credentials,
bounded to ≤10 s, with actionable, sanitised errors, replacing the config-only stubs. The
HTTP path (`POST /v1/sources/{id}/test` → `SourcesValidator` → sources-service credential
decrypt, STORY-06.1/06.2) is unchanged. **upload**: trivial success (no external system /
credentials; storage health is `/readyz`, documented). **web_crawl**: one GET of the first
`start_url` through the SSRF-guarded `Doer` — 2xx/3xx ⇒ ok, non-2xx ⇒ "start URL returned
<status>". **sitemap**: fetch AND parse the first sitemap URL (reusing §3a `fetchSitemap`
gzip/size-cap + the `encoding/xml` parser) — actionable errors for unreachable / non-2xx /
non-XML / empty. **api**: `buildAuthedClient` (07.6, incl. lazy oauth2 token fetch) + ONE
request to the first endpoint/base URL — 401/403 (or an oauth2 token 401/403) ⇒
"authentication failed: check credentials", 2xx ⇒ ok, else actionable + `redactURL`. A hard
`context.WithTimeout(ctx, egress.ProbeTimeout)` (10 s) is derived inside every network
`Test`. A shared secret-free classifier `egress.ClassifyError(err, host)` (SSRF-block / DNS
/ timeout / refused / generic) lives in `internal/egress` (imported by all three network
connectors, no cycle) so the four Tests don't duplicate the mapping; it never echoes the
raw error or the URL query (C-4), naming only the non-secret host. TDD: `classify_test.go`,
webcrawl `probe_test.go`/`sitemap_probe_test.go`, api `probe_test.go` RED first, then GREEN
(reachable success, 401⇒credential, DNS/refused, SSRF-block via a loopback URL through the
REAL guard, non-XML/empty sitemap, and a recording `Doer`/`RoundTripper` asserting the
≤10 s deadline is applied). Hermetic — httptest + the real egress guard, no DB/object
storage. `go test ./...` green; coverage `egress` 87.8% / `webcrawl` 79.9% / `api` 80.5% /
`upload` 87.5% / `connector` 85.4% (≥ 70 % gate); gofmt + `go vet` clean; changed/new files
lint-clean. No migration, no OpenAPI change, no new dependency. *Flagged boundary — now
RESOLVED (ISSUE-0026, ADR-0050 follow-up):* the `/test` HTTP handler had genericised a
non-sentinel service error to a 500, so the actionable message was delivered/tested at the
connector boundary but not surfaced through the `/test` response envelope. The follow-up
fix wraps a failed `Connector.Test` in `sources.ValidationError` so `POST
/v1/sources/{id}/test` returns **400 `validation`** with the connector's sanitised,
actionable message (422 rejected for consistency with the create path); the seam sentinels
(unregistered kind → 404 seam, unknown source → 404) and the credential decrypt/zero
lifecycle are unchanged, and the `sourceTest` OpenAPI operation gains a 400 response
(drift/contract guards green). This completes FR-SRC-14 end-to-end.

**Delivered (STORY-07.9):** tenant-admin-facing connector reference docs
(`docs/connectors/`, ISSUE-0027) — an index (`README.md`) plus one page per kind
(`upload`, `web_crawl`, `sitemap`, `api`), each with a config reference AND a
copy-pasteable example grounded in the code as built (config structs / JSON Schemas /
defaults, credential key names, egress and size/timeout/retry limits). Two doc-vs-code
gaps were documented as "Spec-vs-code note" rather than changed: the web_crawl
`withDefaults` values differ from SPEC-04 §2's illustrative example (`3/1000/0/4` vs
`5/5000/500/8`), and `settings.limits.max_pages_per_crawl` exists in tenant settings
but is not yet consulted by the crawler (the effective cap is per-source `max_pages`).
Docs-only: `go build ./...` green, no code/schema/OpenAPI/migration change. **EPIC-07
is complete (39/39 pts).**

## EPIC-08 · Retrieval and answering — 🚧 15/39 pts

| Key | Story | Pts | Status | Traces |
|---|---|--:|---|---|
| STORY-08.1 | Hybrid retrieval query | 8 | ✅ Done | FR-RET-01/02/08, ADR-0007, ADR-0051, SPEC-06 §2 |
| STORY-08.2 | Retrieve endpoint | 2 | ✅ Done | FR-RET-08, ADR-0052, SPEC-06 §2, SPEC-07 §2e |
| STORY-08.3 | Reranker interface and providers | 5 | 🔲 Todo | FR-RET-03 |
| STORY-08.4 | LLM provider interface | 5 | ✅ Done | NFR-MNT-02, NFR-REL-04 |
| STORY-08.5 | Prompt assembly, citations and grounding refusal | 8 | 🔲 Todo | FR-RET-04/05, SPEC-06 §4–5 |
| STORY-08.6 | Query endpoint with streaming | 5 | 🔲 Todo | FR-RET-06, SPEC-06 §6 |
| STORY-08.7 | Conversation history and question rewrite | 3 | 🔲 Todo | FR-RET-07 |
| STORY-08.8 | Query log and feedback | 3 | 🔲 Todo | FR-RET-09/10 |

**Delivered (STORY-08.4):** the LLM provider seam — a new `internal/llm` package (NFR-MNT-02, NFR-REL-04,
SPEC-06 §5.1, ADR-0053), the pure client library the answering stage generates through. **Built before
STORY-08.3 (a deliberate, user-approved reorder — the LLM-based reranker depends on this seam).** A
provider-neutral `Provider` exposes non-streaming `Complete` and streaming `Stream` (a pull iterator: `Recv →
Event`, `io.EOF` at end) over `Request{Model,System,Messages,MaxTokens,Temperature?,TopP?,Effort?}` →
`Response{Text,Usage{InputTokens,OutputTokens},FinishReason,Model}`. Three providers sit behind a `registry`
(a fourth is one file + one entry): **Anthropic via the official `anthropic-sdk-go`** (pinned **v1.9.0** — the
newest release whose `go` directive is ≤ 1.22-compatible; every ≥ v1.9.1 requires go ≥ 1.23, so a newer pin
would force the toolchain bump the story refuses — `go mod tidy` stayed on `go 1.22`), with the SDK's own retry
disabled so our wrapper is the single authority; **OpenAI + OpenAI-compatible (vLLM/Ollama) via raw
`net/http`** on `/v1/chat/completions`, one implementation parameterised by base URL (no OpenAI SDK, C-2).
Streaming works for all three (Anthropic SDK SSE; OpenAI SSE `data:` with `stream_options.include_usage`),
surfacing a uniform delta/done shape 08.6 will emit. Resilience (NFR-REL-04) — bounded exponential backoff
honouring `Retry-After` on 429/5xx (other 4xx terminal) + a per-provider circuit breaker — is **reused from
`internal/ingest/embed` (ADR-0037), copied not shared** (ponytail: extract `internal/resilience` on a third
consumer). Provider-normalised token `Usage` rides every response/terminal event for 08.5 to fold into
`usage_daily` (ADR-0024). A **two-level fail-closed allowlist** gates access: the provider must be in
`settings.providers_allowed` (SPEC-09 §2) and, when set, the model must match `settings.llm.models_allowed`
(exact or `gpt-*` wildcard). Sampling is sent only to OpenAI (current Claude models reject it); thinking is not
hardcoded (`Effort` → OpenAI `reasoning_effort`, a no-op on Anthropic v1.9.0). Default answer model →
`claude-sonnet-5`; per-provider platform keys `ANTHROPIC_API_KEY`/`OPENAI_API_KEY`/`OPENAI_BASE_URL` (C-4,
never logged; errors carry only the sanitised HTTP status, never prompt content). Only `settings_defaults.json`
/`settings_schema.json` (new `llm.models_allowed`) and `internal/config`/`.env.example` changed — no migration,
no OpenAPI change, drift/validation green. TDD throughout with a hermetic `httptest` suite (the SDK pointed at
the test server via `option.WithBaseURL`): per provider — non-streaming + streaming text/usage/finish, request
shape, retry-then-succeed on 5xx/429, terminal-400-not-retried + error sanitisation, breaker opens +
short-circuits, provider + model allowlist fail-closed, unknown provider, factory key selection. Like the
embedding provider (ADR-0037), the golden-path **e2e is deferred to the query endpoint (STORY-08.6)** since
`internal/llm` has no HTTP/worker path of its own. ADR-0053, ISSUE-0030.

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

_EPIC-04 (Public API surface) is **complete (21/21)**: STORY-04.1 stands up the public router and mounts the accumulated
EPIC-03 middleware in the SPEC-07 §1 order (request ID/logging → recovery → CORS globally, with auth →
credential-keyed rate limit per route) plus every 03.x handler built behind it — the router-wiring seam every
EPIC-02/03 story deferred to. STORY-04.2 then generates the OpenAPI 3.1 spec from that router's route table,
serves it at `/v1/openapi.json`, and adds the drift-guard + jsonschema contract tests (SPEC-07 §3, ADR-0028).
STORY-04.3 then adds the sources
endpoints (`internal/cp/sources`) over the control-plane pool, with the connector framework and the job worker as
injected seams for EPIC-06/09 (ADR-0029). STORY-04.4 adds the documents endpoints (`internal/documents`) — the
first request path to reach tenant content, through a `tenant.DB` from the resolver (ADR-0030). STORY-04.5 adds
the jobs endpoints (`internal/cp/jobs`) over the control-plane pool, with queued-job cancellation effective now
and running-job cancellation as the EPIC-09 River `Canceller` seam (ADR-0031). STORY-04.6 adds the admin tenant
endpoints (`internal/cp/tenants`) — the platform-admin lifecycle surface over the existing provisioner/lifecycle,
with the async River provision/delete *execution* as the one EPIC-09 seam (ADR-0032) — closing out the epic.
Suggested next: EPIC-05 (Ingestion pipeline), the first epic to consume the documents/jobs request paths
04.4/04.5 stood up._
