# SPEC-09: Security

**Implements:** NFR-SEC-01..07, FR-SRC-10, FR-ACC-05, C-4 · **Decisions:** ADR-0001, ADR-0003

## 1. Isolation
- Database per tenant (ADR-0001); distinct Postgres role per tenant database with privileges only on that database.
- Application connects with the tenant role, never a superuser, for data-plane operations. Provisioning/migration uses a separate privileged role held only by the `ragctl` process.
- `tenant.DB` is the only path to tenant data (ADR-0003); `golangci-lint` custom rule forbids `Unsafe()` and raw `pgxpool` outside `internal/tenant` and `cmd/ragctl`.
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
