# ISSUE-0016: Credential encryption and handling

**Type:** Feature · **Status:** Done · **Story:** STORY-06.2 · **Traces:** FR-SRC-10, SPEC-04 §6, SPEC-09 §2, ADR-0041

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-06.2 for traceability; the backlog
> story remains the authoritative work item. STORY-06.2 advances EPIC-06 (8/13).

## Summary
Make source credentials real (FR-SRC-10, SPEC-04 §6): encrypted on write, never
returned by any API, decrypted only for a Test/Sync and zeroed afterwards, with
sanitised errors. It replaces the STORY-04.3 fail-closed `400` credentials stub with
real encrypt-on-write and threads decrypted credentials into the connector `Test`
seam STORY-06.1 left passing `nil`. The crypto is the existing platform envelope
Cipher (SPEC-09 §2) — no new scheme or dependency.

## Scope
- `internal/crypto/zero.go`: `Zero([]byte)` — wipe a decrypted-secret buffer.
- `internal/cp/sources`: `Encrypter`/`Decrypter` ports on `Service` (satisfied by
  `*crypto.Cipher`, injected in `internal/cli`); seal-on-write in Create/Update
  (plaintext `map[string]string` → ciphertext into `credentials_enc`, store sees
  ciphertext only, plaintext buffer zeroed); `Store.GetCredentials` (the only read of
  `credentials_enc`); decrypt-and-zero in `Test` (`defer clear`); the `Validator.Test`
  seam widened with `creds map[string]string`.
- `internal/connector/registry.go`: `SourcesValidator.Test` forwards the decrypted
  map to `Connector.Test` as `connector.Credentials`.
- `internal/cli/api_server.go`: wire the startup `crypto.Cipher` as the sources
  service `Encrypter`/`Decrypter`.
- Not in scope: the upload connector / `ingest_document` job (STORY-06.3) and
  `Connector.Sync` / any concrete connector (EPIC-07). The decrypt helper is wired to
  the Test/Sync seam; `Sync` is not implemented.

## Resolution
- **Reuse over new (C-4).** Credentials are sealed with the same `crypto.Cipher` the
  resolver/provisioner use (AES-256-GCM DEK wrapped by KMS, SPEC-09 §2), so
  `ragctl keys rotate-dek` covers `credentials_enc` for free. No migration: the column
  already exists.
- **Never returned, structurally.** The public `Source` projection / `sourceColumns`
  keep omitting `credentials_enc`; a dedicated `GetCredentials` is the sole reader,
  used only by the decrypt path.
- **Zeroed after use.** The decrypted JSON `[]byte` is wiped right after unmarshalling
  and the credential map is `clear()`ed via `defer` when `Test` returns. `ponytail:` Go
  strings can't be zeroed in place — the map values become GC-eligible; a
  `[]byte`-valued credential type is the upgrade path (recorded in ADR-0041).
- **Fail closed.** Credentials on the write path with no Encrypter is an error, never a
  plaintext store. **Sanitised errors:** crypto/decrypt errors carry no ciphertext or
  secret; a connector `Test` error maps to the generic envelope; credentials are never
  logged.
- **No OpenAPI change:** the generator does not model request-body properties, so the
  `credentials` write field is not in the served spec (drift guard stays green).

## Verification
- TDD throughout (tests watched red before implementation): `crypto.Zero`; sources
  encrypt-on-write, fail-closed, decrypt-and-zero, zeroed-after-use, sanitised-error;
  connector creds-forwarding; handler seal + non-string-rejection.
- `mise run test`: all packages green except the pre-existing `internal/cli` env-only
  reds (mise `.env` injection leaks `CONTROL_PLANE_URL`/age-key; pass with clean env).
- `mise run lint`: the one new finding (unused test param) fixed; the remaining 5 are
  the pre-existing golangci-lint/go1.26 toolchain drift in untouched files.
- Coverage: `internal/connector` 85.4% (gate 70%); sources store SQL is e2e-covered.
- e2e (`test/e2e/credentials_e2e_test.go`) over the real control-plane Postgres and the
  **real** Cipher: create-with-credentials → 201 with no secret echoed; the stored
  `credentials_enc` is ciphertext (asserted != plaintext, round-trips via the cipher);
  GET returns no credentials; `/test` decrypts and hands the plaintext to the connector.
  `TestSourcesGoldenPath`/`TestConnectorFrameworkGoldenPath` updated and green (the
  psql-based job assertion trips the wedged `docker compose exec`, ISSUE-0014 — the
  credential assertions pass).
