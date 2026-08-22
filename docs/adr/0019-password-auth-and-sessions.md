# ADR-0019: Password authentication and server-side sessions — argon2id, hashed session tokens, double-submit CSRF

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-ACC-01, SPEC-09 §3, SPEC-02 §2/3, C-3, C-4/ADR-0012

## Context
STORY-03.1 opens EPIC-03 (control-plane services): email/password signup and login, server-side sessions with a cookie, logout, the account lockout policy, and CSRF on mutations (FR-ACC-01, SPEC-09 §3). The control-plane schema had `users` but no password column and no session storage, and the public HTTP router does not exist yet (STORY-04.1, EPIC-04). Prior epics built the service/repository layer fully against the real control-plane Postgres and deferred only the HTTP route wiring; this story mirrors that.

Open sub-decisions:
1. **Where auth lives.** SPEC-02 §2 names `internal/cp/auth` as the package owning users/members/api_keys with password/key hashing.
2. **Password hashing parameters** (SPEC-09 §3 says argon2id).
3. **Session storage mechanism** — SPEC-02 §3 allows a Postgres table or Redis.
4. **How to store the session token** so a leaked control-plane snapshot is not a set of live sessions.
5. **CSRF mechanism** and where it is enforced given there is no router yet.

## Decision
- **A new `internal/cp/auth` package** holds the auth service, exactly as SPEC-02 §2 names it. It depends only on `pgx` (via a narrow `DB` interface satisfied by `*pgxpool.Pool` through `FromPool`) and `golang.org/x/crypto/argon2`, so it is unit-testable with a fake DB and integration-tested against real Postgres. It touches no tenant data (C-3): users and sessions are control-plane-only.
- **argon2id with PHC-encoded parameters.** `HashPassword` uses argon2id (t=3, m=64 MiB, p=4, 16-byte random salt, 32-byte key) and encodes the parameters into the `$argon2id$v=19$m=..,t=..,p=..$salt$hash` string, so raising the cost later still verifies old hashes. `VerifyPassword` recomputes with the embedded parameters and compares in constant time. The plaintext is never stored or logged; the length floor is enforced now and the SPEC-09 §3 breach-list check is left as a follow-up hook.
- **Server-side sessions in a Postgres `sessions` table**, not Redis: the control-plane Postgres already exists, the admin-UI session volume is low, and keeping state in one store avoids a new dependency and a second consistency boundary. Revisit if session throughput ever justifies Redis.
- **The session cookie carries a 128-bit random id; only its sha256 is stored** (`sessions.token_hash`). Lookup hashes the presented cookie and matches on the hash, so a leaked snapshot cannot be replayed as a cookie. `idle_expires_at` enforces the 12 h idle timeout and is slid forward on each use; logout sets `revoked_at`; a revoked or expired token fails `Lookup` with a single opaque `ErrNoSession` (no enumeration oracle).
- **Lockout on the `users` row.** `failed_login_count` and `locked_until` implement 10 failures / 15 min (SPEC-09 §3). A locked account is refused *before* the password is checked, and the correct password is still refused inside the window; a wrong attempt increments the counter (and may set the lock), a correct attempt resets both. Login collapses unknown-email and wrong-password into `ErrInvalidCredentials` so it does not disclose which emails exist.
- **CSRF is a per-session double-submit token** (`sessions.csrf_token`), returned in the login response body for the SPA to echo in an `X-CSRF-Token` header on mutating methods; safe methods (GET/HEAD/OPTIONS) are exempt. The comparison is constant-time and rejects empty tokens. The cookie is HttpOnly + SameSite=Lax (Secure in production), so SameSite is the first line of defence and the double-submit token the second.
- **Handlers/middleware unit-tested with `net/http/httptest`; router wiring deferred to STORY-04.1.** `Handlers` (Signup/Login/Logout), `RequireSession`, and `CSRF` are real `http.Handler`s exercised end to end with `httptest` (no full server). Mounting them on the public router is EPIC-04, mirroring the enroll/suspend/move deferrals (ADR-0016/0017). No scope was invented; the deferral is recorded, not stubbed.
- **Schema via goose migration mirrored into the schema file.** Control migration `00004_users_auth_and_sessions.sql` adds the password columns and the `sessions` table; the same DDL is appended to `schemas/control_plane.sql` so the STORY-01.5 drift guard stays green.

## Consequences
- Auth is a self-contained control-plane service that the EPIC-04 router mounts with no changes to `internal/tenant` (still the data-plane-only path, ADR-0003).
- Sessions are Postgres-backed; if throughput ever demands it, the `DB` interface makes a Redis-backed store a drop-in without touching the service logic.
- New table `sessions` and three `users` columns; `job_kind`/tenant schemas are untouched. `golang.org/x/crypto` is promoted to a direct dependency.
- The breach-list password check and OIDC login (PKCE/nonce/issuer allowlist) remain open, tracked by SPEC-09 §3 and STORY-03.2.
- The pure helpers (argon2id hashing/verify, token/CSRF generation and hashing, lockout policy, the service's branch logic via a fake DB) are unit-tested; the signup → login → lookup → logout golden path and the lockout-after-10-failures path are proven end to end against the real control-plane Postgres.
