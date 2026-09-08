# ISSUE-0042: DEK rotation command

**Type:** Feature · **Status:** Done · **Story:** STORY-10.4 · **Traces:** NFR-SEC-03, SPEC-09 §2, ADR-0001, ADR-0065

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs
> (`docs/adr/`). This issue records STORY-10.4 for traceability.

## Summary
`ragctl keys rotate-dek` re-encrypts all stored secrets under a new DEK version with zero
downtime; the old key is retained until completion (NFR-SEC-03). The enabler is a new
`crypto.Keyring` (primary + previous versions) threaded through the app, so the running
fleet decrypts both the old and new versions while rows are migrated.

## Scope
- `internal/crypto/keyring.go`: `Keyring` (Encrypt→primary, Decrypt→by version, Reencrypt
  idempotent) + `Cipher.Version()`.
- `internal/cli/keys.go`: `keys new-dek` (generate + KMS-wrap the next version) and
  `keys rotate-dek` (re-encrypt `tenant_databases.password_enc` + `sources.credentials_enc`
  to the primary version, idempotent/resumable); registered in the CLI grammar.
- `internal/config` + `internal/cli/secrets.go`: `DEK_PREVIOUS` config + `LoadStartupKeyring`;
  `serve`/`work`/`migrate tenants`/`enroll`/`tenant` now load a keyring (a single-key
  keyring when no previous keys are set — a drop-in for the old Cipher).
- Docs: ADR-0065, this issue, backlog. (Runbook procedure is STORY-10.8.)
- Not in scope: automated rolling-restart orchestration; a batched/parallel rotation.

## Resolution
- **Keyring:** seals new secrets under the primary version, decrypts any held version,
  fails closed on an unknown version. Satisfies the existing Encrypt/Decrypt interfaces,
  so consumers (resolver/sources/provisioner) are unchanged.
- **Zero downtime:** `DEK_PREVIOUS` keeps the old key in the ring during the window;
  procedure = new-dek → rolling restart (new primary + old previous) → rotate-dek → drop
  previous.
- **Idempotent/resumable:** `Reencrypt` skips a field already at the primary version;
  a re-run finishes only the remainder.
- **No key leakage:** only the KMS-wrapped blob is written (0600); DEK plaintext is zeroed.

## Tests
- Unit (`internal/crypto`, hermetic): the keyring seals under the primary and decrypts any
  version, fails closed on an unknown version; `Reencrypt` is idempotent (v1 field → v2,
  re-run a no-op) and a no-op on empty.
- e2e (`test/e2e/keys_rotate_e2e_test.go`, real binary + Postgres): a tenant password
  (sealed v1 by enroll) and a source credential (sealed v1) both become v2 ciphertext that
  decrypts to the originals after `keys rotate-dek` with primary v2 + previous v1; a second
  rotation is a no-op.
- Build/vet green (`-tags e2e`); full unit suite green (keyring rewire regression-free).
