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
STORY-04.1. OIDC (PKCE, nonce, issuer allowlist) is STORY-03.2.

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
