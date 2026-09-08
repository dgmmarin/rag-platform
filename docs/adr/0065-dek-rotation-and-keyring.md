# ADR-0065: DEK rotation — a multi-version keyring for zero-downtime re-encryption, driven by `ragctl keys`

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** NFR-SEC-03, SPEC-09 §2 · **Decisions:** ADR-0001

## Context
NFR-SEC-03 / SPEC-09 §2: the data-encryption key (DEK) that seals stored secrets — tenant
database passwords (`tenant_databases.password_enc`) and source credentials
(`sources.credentials_enc`) — must be rotatable, re-encrypting all secrets under a new
key version with **zero downtime**, the old key retained until completion. The envelope
format already carries a 2-byte key-version header (ADR-0001), and secrets are sealed by
a single `crypto.Cipher` bound to one version — which cannot open a row sealed under a
different version. So a rotation that migrates rows one at a time needs the fleet to hold
both keys during the window.

## Options / decisions
- **A `crypto.Keyring` holds a primary Cipher plus previous-version Ciphers, and drops in
  wherever a `*Cipher` was used.** It seals new secrets under the primary and decrypts any
  version it holds, satisfying the same `Encrypt`/`Decrypt` interfaces the resolver,
  sources and provisioner already depend on — so `serve`, `work` and the one-shot commands
  switch from a single Cipher to a keyring with no change to those consumers. A
  single-key deployment is just a keyring with no previous keys, behaviourally identical
  to before.
- **Zero downtime = the fleet decrypts both versions during the window.** Config gains
  `DEK_PREVIOUS` (a list of `path:version`), loaded into the keyring alongside the primary.
  The rotation procedure is: `keys new-dek` mints and KMS-wraps the next version →
  operators deploy the fleet with the new key as primary and the old in `DEK_PREVIOUS`
  (a rolling restart, so every process now seals under the new version and decrypts
  either) → `keys rotate-dek` re-encrypts every stored secret to the primary → operators
  drop the old key from `DEK_PREVIOUS`. At no point is a row unreadable by the running
  fleet.
- **`keys rotate-dek` is idempotent and resumable.** Each secret's version header is
  checked; a row already at the primary version is skipped, so an interrupted rotation
  re-runs to finish only the remainder. `Keyring.Reencrypt` decrypts with the matching
  key and re-seals under the primary, zeroing the recovered plaintext. Rows are updated
  one statement at a time — a failure leaves earlier rows migrated (safe, because the
  command is re-runnable).
- **Key material is never printed or logged.** `keys new-dek` writes only the KMS-wrapped
  blob (0600); the DEK plaintext lives only inside a Cipher and is zeroed after use.

## Consequences
- A DEK can be rotated on a live platform without downtime and without a big-bang
  re-encryption, one row class at a time, resumable if interrupted.
- The keyring is now the crypto object threaded through the app; adding a future secret
  column to rotate is a one-line addition to `keys rotate-dek`.
- The single-process `keys rotate-dek` re-encrypts sequentially; a very large fleet of
  tenants means many small UPDATEs (bounded by the row count, not data size). Acceptable
  for the control plane's row counts; a batched/parallel variant is a later option.
- The operational sequence (new-dek → rolling restart → rotate-dek → drop previous) is the
  one thing an operator must follow in order; it is documented on the `keys new-dek`
  command and belongs in the key-rotation runbook (STORY-10.8).
