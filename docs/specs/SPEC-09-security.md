# SPEC-09: Security

**Implements:** NFR-SEC-01..07, FR-SRC-10, FR-ACC-05, C-4 · **Decisions:** ADR-0001, ADR-0003, ADR-0018

## 1. Isolation
- Database per tenant (ADR-0001); distinct Postgres role per tenant database with privileges only on that database. Provisioning revokes the default `CONNECT` grant to `PUBLIC` on each tenant database and grants it back only to the owning role (SPEC-01 §6.2a, ADR-0018), so no other tenant's role can open a session against it — the connection-level boundary NFR-SEC-01 requires.
- Application connects with the tenant role, never a superuser, for data-plane operations. Provisioning/migration uses a separate privileged role held only by the `ragctl` process.
- `tenant.DB` is the only path to tenant data (ADR-0003); a `golangci-lint` `forbidigo` rule forbids `tenant.DB.Unsafe()` (the raw `*pgxpool` escape hatch) outside the privileged provisioning/migration packages `internal/provision` and `internal/migrate` (ADR-0018, STORY-02.6), failing CI on any other caller.
- Isolation test suite (SPEC-01 §9) in CI.

## 2. Secrets
- Envelope encryption: a data-encryption key (DEK) encrypts `tenant_databases.password_enc`, `sources.credentials_enc`, provider keys. DEK is wrapped by a KMS key (AWS KMS / GCP KMS / age for self-hosted). DEK rotation via `ragctl keys rotate-dek` re-encrypts all rows.
- AES-256-GCM with per-row nonce; ciphertext includes key version for rotation.
- Secrets are decrypted into memory only for the duration of use and never logged or returned by APIs.

## 3. Authentication & sessions
- Passwords: argon2id; breach-list check on set; lockout after 10 failures/15 min.
- API keys: 32 bytes random, prefix for lookup, sha256 stored; scopes; optional expiry; `last_used_at` updated at most once per minute.
- Sessions: 128-bit random ID, HttpOnly, Secure, SameSite=Lax, 12 h idle timeout; CSRF double-submit on mutating routes.
- OIDC: PKCE, nonce, issuer allowlist per deployment.

Implemented by `internal/cp/auth` (STORY-03.1, FR-ACC-01, ADR-0019): argon2id password
hashing (PHC-encoded, per-hash random salt, constant-time verify) on `users.password_hash`;
email/password signup and login with the lockout policy backed by `users.failed_login_count`
/`users.locked_until` (a locked account is refused without checking the password, and the
correct password is still refused inside the window). Sessions are server-side in the control
plane (`sessions` table): the 128-bit cookie id is stored only as its sha256 (`token_hash`), so
a leaked snapshot cannot be replayed; `idle_expires_at` enforces and slides the 12 h idle
timeout; logout sets `revoked_at`. The cookie is HttpOnly + SameSite=Lax (Secure in production);
CSRF is a double-submit token minted per session (`sessions.csrf_token`) and required on mutating
methods. The breach-list check is a follow-up (a length floor is enforced now). The HTTP handlers
and middleware are unit-tested with `net/http/httptest`; mounting them on the public router is
STORY-04.1.

OIDC login (STORY-03.2, FR-ACC-01, ADR-0020) is implemented in the same `internal/cp/auth`
package. A configurable provider (`OIDC_ISSUER`/`OIDC_CLIENT_ID`/`OIDC_CLIENT_SECRET`/
`OIDC_REDIRECT_URL` via `internal/config`) drives the authorization-code + PKCE flow: `AuthCodeURL`
mints a per-attempt state, nonce, and PKCE verifier and returns the provider URL carrying the S256
code challenge; `Callback` compares `state` in constant time before any token exchange, verifies the
id_token (signature via JWKS, issuer, audience, expiry, and nonce) using `github.com/coreos/go-oidc/v3`
+ `golang.org/x/oauth2`, and refuses an unverified email. The single configured issuer is the
per-deployment allowlist (the verifier rejects any other `iss`). On success it mints a session through
the SAME session store as password login (no fork). External identities are linked in a
`user_identities` table keyed by `(issuer, subject)`; resolution order is existing-identity →
existing-user-by-verified-email (account linking) → JIT create (when `OIDC_JIT_PROVISIONING=true`,
otherwise refuse). JIT users are password-less. The client secret and all tokens are never logged.
The per-attempt state/nonce/verifier travel in a short-lived HttpOnly cookie between the start and
callback handlers; those handlers are unit-tested with `httptest` and mounted on the public router in
STORY-04.1.

API keys (STORY-03.4, FR-ACC-04/05, ADR-0021) are implemented in the same `internal/cp/auth`
package over the existing `api_keys` table. The wire format is `Authorization: Bearer rk_<prefix>_<secret>`:
`rk_` scheme marker, an 8-char hex prefix stored in the clear (`key_prefix`, indexed lookup + display),
and a 32-byte base64url secret body. Only the sha256 of the FULL presented value is stored (`key_hash`,
reusing the session-token `hashToken`); the plaintext is returned once from `Create` and never persisted
or logged. `APIKeyVerifier` looks a key up by `(key_prefix, key_hash)` with `revoked_at is null`, so
`Revoke` takes effect on the next request; unknown/tampered/revoked/malformed keys all collapse to one
opaque `ErrInvalidKey` (→ 401, no enumeration oracle), and `expires_at` is checked in Go (`ErrKeyExpired`,
also → 401 at the edge). `last_used_at` is stamped at most once per minute (throttled; a failed stamp is
non-fatal). Scopes are exactly `query`, `ingest`, `admin` (SPEC-07 §2); a closed typed set rejects any
other, and `RequireScope` middleware authenticates the Bearer key, enforces the scope (403 on a miss),
and injects the key's tenant into the request context (FR-ACC-03 — derived from the credential, never a
client parameter). The service and middleware are unit-tested (fake DB + `httptest`) and proven end to
end against real Postgres; router wiring is STORY-04.1.

## 4. Crawler safety (SSRF)
- Resolve DNS, reject RFC 1918, loopback, link-local, metadata IPs (169.254.169.254), and re-validate on each redirect hop.
- Allowlist enforced on scheme+host+path prefix; max response size 20 MB; timeouts 30 s.
- Outbound egress from workers ideally via a proxy with the same deny rules.

## 5. Provider data handling
- `settings.providers_allowed` gates which embedding/LLM providers a tenant's data may be sent to.
- Provider requests include no tenant identifiers beyond what the provider needs; logs redact prompt content above a configurable size unless debug is enabled for a tenant by a platform admin (audited).

## 6. Transport and platform
- TLS everywhere including Postgres (`sslmode=verify-full` in production).
- Containers run as non-root; read-only filesystem; minimal base images.
- Dependency scanning (govulncheck, pip-audit) and image scanning in CI.

## 7. Data lifecycle
- Tenant deletion drops database and role; object-storage prefix `tenants/<id>/` deleted; backups age out per retention; deletion recorded in audit log with completion timestamp (GDPR evidence).
- Document soft delete → hard delete after 30 days (SPEC-03 §4).

## 8. Threat model summary
| Threat | Control |
|---|---|
| Cross-tenant read via API bug | per-database isolation, TenantDB, isolation tests |
| Stolen API key | scopes, expiry, revoke, rate limits, audit |
| Malicious tenant uses crawler against internal network | SSRF guards, egress proxy |
| Leaked DB snapshot | secrets encrypted with external KMS; content encrypted at rest by storage |
| Prompt injection in crawled content | answers are grounded and cited; model instructed to treat sources as data; no tool execution from content |
