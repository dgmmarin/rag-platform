# ADR-0041: Source credential encryption — seal on write, decrypt-and-zero only for Test/Sync, reusing the platform envelope Cipher

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-10, FR-ACC-03, C-3, C-4, SPEC-04 §6, SPEC-09 §2 · **Decisions:** ADR-0003, ADR-0016, ADR-0029, ADR-0040

## Context
STORY-06.2 must make source credentials real: encrypted on write, never returned
by any API, decrypted only inside a sync (and the "test connection"), and zeroed
after use, with sanitised error messages (FR-SRC-10, SPEC-04 §6).

The seams already exist. STORY-04.3 (ADR-0029) shipped the sources API with the
`credentials` write field rejected `400` (fail closed — no plaintext on the write
path until this story). STORY-06.1 (ADR-0040) shipped the connector framework whose
`Connector.Test(ctx, cfg, creds)` and `Credentials` (`map[string]string`) already
take credentials, but the `SourcesValidator.Test` seam passed `nil`. The
`sources.credentials_enc bytea` column already exists (`schemas/control_plane.sql`),
and the platform envelope Cipher (`internal/crypto`, AES-256-GCM DEK wrapped by KMS,
SPEC-09 §2) is already used by the resolver, provisioner and tenant move to seal the
per-tenant DB password — and is reserved at API startup.

Out of scope (later stories): the upload connector / `ingest_document` job
(STORY-06.3) and every concrete connector plus `Connector.Sync` (EPIC-07). Since no
connector implements `Sync` yet, this story wires the decrypt path up to the
Test/Sync seam but does not implement `Sync`.

## Options
- **Crypto scheme.** (a) Introduce a credential-specific scheme/dependency —
  rejected: the envelope Cipher (SPEC-09 §2) already encrypts secret columns; a
  second scheme is duplicate surface and a rotation liability. (b) Reuse
  `crypto.Cipher` exactly as the resolver/provisioner do (chosen) — same DEK,
  same versioned ciphertext layout, so `ragctl keys rotate-dek` covers
  `credentials_enc` for free.
- **Where encryption lives.** (a) In the store (SQL layer) — rejected: the store is
  the DB boundary and should write bytes, not hold crypto. (b) In the sources
  service (chosen): the handler passes plaintext `map[string]string`, the service
  seals it, zeroes the plaintext JSON buffer, nils the plaintext map, and hands the
  store only the ciphertext (`CredentialsEnc []byte`). The store never sees plaintext.
- **Where the ciphertext is read.** The public `Source` projection and `sourceColumns`
  keep omitting `credentials_enc` (FR-SRC-10); a dedicated `Store.GetCredentials` is
  the *only* read of the column, used solely by the decrypt path — so "never returned"
  is structural, not a per-handler discipline.
- **Threading credentials to the connector without coupling.** The sources `Validator`
  seam (ADR-0029) must stay connector-import-free. (a) Reference `connector.Credentials`
  in sources — rejected (coupling). (b) Widen the seam to
  `Test(ctx, kind, cfg, creds map[string]string)` and let the `SourcesValidator`
  adapter convert `map[string]string` → `connector.Credentials` (chosen). The sources
  service owns decrypt + zero; the adapter only forwards.
- **"Zeroed afterwards" for a `map[string]string`.** Go strings cannot be overwritten
  in place. (a) Change `Credentials` to `map[string][]byte` across the frozen connector
  interface — rejected: churns the NFR-MNT-01 contract for a marginal gain. (b) Zero the
  decrypted `[]byte` buffer immediately after unmarshalling and `clear()` the map via a
  `defer` the moment Test/Sync returns (chosen), marked `ponytail:` with the string-bytes
  limitation and the upgrade path. A new `crypto.Zero([]byte)` helper does the wipe.
- **Fail-closed on a missing Encrypter.** Credentials on the write path with no
  Encrypter wired is an error, never a silent plaintext store (C-4).
- **Error sanitisation.** Crypto/decrypt errors are constructed without the ciphertext
  or any secret value; a connector `Test` failure maps to the generic public envelope
  (`writeServiceError` default), never the raw error; credentials are never logged.

## Decision
Add credential handling to `internal/cp/sources`, reusing `crypto.Cipher` (injected
in `internal/cli` as the service's `Encrypter`/`Decrypter`, the same cipher the
resolver/provisioner use). Create/Update seal `credentials` into `credentials_enc`
(store sees ciphertext only); the public projection never selects the column; `Test`
reads it via `Store.GetCredentials`, decrypts into a map, zeroes the decrypted buffer,
passes the map through the widened `Validator.Test(…, creds)` seam to the connector,
and clears the map on return. `connector.SourcesValidator.Test` forwards the map as
`connector.Credentials`. Add `crypto.Zero([]byte)`. The public `credentials` body
field is a flat `map[string]string` (shape validated by JSON decode). No schema
migration (the column exists) and no OpenAPI change (request-body properties are not
modelled by the generator).

## Consequences
- Credentials are encrypted on write, never returned, decrypted only for Test (and
  the future sync worker via the same helper), and cleared after use — FR-SRC-10 /
  SPEC-04 §6 satisfied, and the fail-closed `400` stub from STORY-04.3 is replaced by
  real encrypt-on-write.
- DEK rotation (`ragctl keys rotate-dek`, SPEC-09 §2) already covers
  `credentials_enc` because the ciphertext is the standard versioned envelope layout.
- The connector `Credentials` type stays `map[string]string`; the string-value zeroing
  limitation is documented with a `[]byte`-valued upgrade path if a stronger guarantee
  is ever required.
- Adding a connector still needs only its package + a `Register` call: the credential
  lifecycle is entirely in the sources service and the adapter (NFR-MNT-01 preserved).
- `internal/connector` stays at 85.4% (gate 70%); the sources store SQL for
  `credentials_enc` is covered by `test/e2e/credentials_e2e_test.go` against the real
  Postgres and the real Cipher (sources is not a coverage-gated package).
