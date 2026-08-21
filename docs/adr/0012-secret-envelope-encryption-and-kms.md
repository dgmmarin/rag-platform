# ADR-0012: Secret envelope encryption format and KMS providers

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** C-4, NFR-SEC-01, SPEC-09 §2 · **Decisions:** ADR-0002, ADR-0009

## Context
STORY-01.4 introduces secret loading. Per C-4 / SPEC-09 §2, platform secrets
(`tenant_databases.password_enc`, `sources.credentials_enc`, provider keys) are
protected with envelope encryption: a data-encryption key (DEK) encrypts the
secret columns, and the DEK itself is wrapped by a KMS key held outside the
database. The DEK must load at startup, a missing DEK must fail startup, and the
ciphertext must carry a key version so a later rotation (`ragctl keys rotate-dek`,
STORY-10.4) can find rows sealed under an old DEK without decrypting them.

Two decisions had to be pinned now: the on-disk ciphertext layout (changing it
later would be a migration) and how the DEK is wrapped for self-hosted vs cloud
deployments.

## Options
1. Store a bare AES-GCM ciphertext (nonce prepended) with no version. Simplest,
   but rotation cannot tell which DEK generation sealed a row without trial
   decryption — a non-starter for `rotate-dek`.
2. Wrap the whole record in a self-describing container (e.g. JSON with base64
   fields, or the full age format per secret). Flexible but bloats every secret
   column and couples the row format to a library's framing.
3. A fixed compact binary layout with an explicit key-version header, over
   AES-256-GCM, and a small `KMS` interface for wrapping the DEK with pluggable
   backends (age for self-hosted, AWS KMS for cloud).

## Decision
Option 3.

**Ciphertext layout v1** (`internal/crypto`), big-endian:

```
[ version : 2 bytes ][ nonce : 12 bytes ][ AES-256-GCM output (ciphertext||tag) ]
```

- AES-256-GCM; DEK is exactly 32 bytes (rejected otherwise). Per-message random
  12-byte nonce, so a key is never reused with the same nonce.
- The 2-byte version is the DEK generation. `KeyVersion(ciphertext)` reads it
  without a key, so rotation can select old-version rows cheaply. Decrypt refuses
  a version that does not match the cipher's generation (fail closed).
- The GCM tag authenticates the payload; a flipped byte fails `Open` rather than
  returning corrupted plaintext (tamper detection). The version+nonce header is
  not fed as AAD in v1 — the version is validated explicitly before Decrypt and
  the nonce is inherent to GCM; a future v2 may bind the header as AAD if needed.

**KMS providers.** A two-method `KMS` interface (`Wrap`/`Unwrap` the DEK):
- `local` — age X25519 identity (`filippo.io/age`). The age secret key is the
  KMS key, supplied out-of-band (mounted secret / env), never in Postgres. Chosen
  for self-hosted and local dev because it needs no cloud dependency and is a
  well-reviewed file-encryption primitive.
- `aws` — AWS KMS `Encrypt`/`Decrypt` wrap/unwrap the DEK; the KMS key never
  leaves AWS. Real API wiring over a narrowed `kmsAPI` seam so it is unit-testable
  with a fake (no cloud creds in CI); the golden-path unit + e2e tests run on
  `local`.

**Startup wiring.** `crypto.LoadDEK` unwraps the wrapped DEK via the configured
KMS and returns a `Cipher` bound to the active key version. It fails closed on a
missing wrapped DEK, an unwrap failure, or a wrong-size key. `ragctl serve` calls
`LoadStartupCipher` before its stub body, so a missing/unwrappable DEK is a real
error (exit 1 per ADR-0010), never a silent start. Config comes from
`internal/config` (env with optional file overlay, ADR-0009 precedence). DEK
plaintext lives only inside the `Cipher`; no key material is logged (SPEC-09 §2).

**Toolchain / dependency pinning.** The AWS SDK for Go v2 latest releases require
Go >= 1.24, but the repo pins Go 1.22 (`mise.toml`). To keep that pin, the AWS
SDK is pinned to the last Go-1.22-compatible line (`aws-sdk-go-v2 v1.36.3`,
`service/kms v1.38.3`, `config v1.29.14`) and `filippo.io/age` to `v1.2.1` (1.3.x
requires Go 1.24). Upgrading past these implies a Go-toolchain bump and is
deferred to a deliberate change.

## Consequences
- Rotation (STORY-10.4) is possible without re-reading key material: select rows
  by `KeyVersion`, decrypt under the old-generation cipher, re-encrypt under the
  new. No format migration needed for v1 → future versions because the header is
  self-describing.
- Adding a provider (e.g. GCP KMS) is a new `KMS` implementation plus one case in
  `buildKMS`; no change to the ciphertext format or callers (NFR-MNT-01/02).
- The 14-byte header overhead per secret is negligible for the secret columns it
  protects.
- Any future change to the layout must bump the version byte and ship a reader
  for both, never silently reinterpret v1 bytes.
- Dependency pins are conservative to preserve the Go 1.22 pin; a toolchain bump
  is a separate, intentional decision.
