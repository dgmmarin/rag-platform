# ADR-0021: API key format, scopes, and verification — `rk_<hexprefix>_<secret>`, sha256-at-rest, SQL-side revocation

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-ACC-04, FR-ACC-05, SPEC-02 §3, SPEC-07 §2, SPEC-09 §3, C-3, C-4

## Context
STORY-03.4 adds programmatic credentials to the control-plane auth package
(`internal/cp/auth`, SPEC-02 §2): create/list/revoke API keys, and an
authentication verifier that resolves a presented key to its tenant and scopes
and enforces scope and expiry. The `api_keys` table already exists
(`schemas/control_plane.sql`, control migration `00001`): `key_hash bytea unique`,
`key_prefix text`, `scopes text[]`, `expires_at`, `revoked_at`, `last_used_at`.
SPEC-02 §3 fixes the wire format as `Authorization: Bearer rk_<prefix>_<secret>`,
lookup by prefix, constant-time sha256 compare; SPEC-09 §3 fixes 32-byte secret
entropy, a stored hash, scopes, optional expiry, and `last_used_at` "updated at
most once per minute". The public router does not exist yet (STORY-04.1), so —
mirroring STORY-03.1/03.3 (ADR-0019) — the service and middleware are built and
tested (unit + real-Postgres e2e) with route wiring deferred.

Open sub-decisions this ADR records:
1. **The encoding of each segment** of `rk_<prefix>_<secret>`.
2. **What is hashed and stored**, and how a presented key is matched.
3. **How revocation and expiry are honoured** (SQL filter vs. Go check) and how
   failures are reported so they cannot be used to enumerate keys.
4. **The scope vocabulary** and where it is enforced.

## Decision
- **Segments: `rk_` scheme, an 8-char hex prefix, a base64url secret body.** The
  scheme marker `rk_` makes a leaked key recognisable to secret scanners. The
  prefix is 4 random bytes rendered as **hex**, not base64url: the base64url
  alphabet includes `_`, which is the segment separator, so a base64url prefix
  could contain a `_` and be silently truncated when the value is parsed. Hex
  cannot, so the prefix is always recovered intact by `SplitN(value, "_", 3)`.
  The secret body is 32 bytes (SPEC-09 §3) of base64url and *may* contain `_` —
  harmless, because `SplitN(_, 3)` keeps the tail whole. The prefix is stored in
  the clear (`key_prefix`) for an indexed lookup and for display in listings.
- **Only sha256(full presented value) is stored** (`key_hash`), reusing the
  session-token hashing approach (`hashToken`) from STORY-03.1: the plaintext
  secret is returned exactly once from `Create` and never persisted or logged
  (FR-ACC-05, C-4). Verification hashes the *entire* `rk_<prefix>_<secret>` value
  and matches on `(key_prefix, key_hash)`, so a leaked control-plane snapshot is
  a set of hashes, not usable keys.
- **Revocation is enforced in SQL; expiry is checked in Go.** The verifier's
  lookup carries `and revoked_at is null`, so `Revoke` (which stamps
  `revoked_at`) takes effect on the very next request with no cache to invalidate.
  A revoked, unknown, tampered, or malformed key all collapse to one opaque
  `ErrInvalidKey` (→ 401) so a caller cannot distinguish them and enumerate keys,
  exactly as sessions collapse to `ErrNoSession`. `expires_at` is fetched and
  compared in Go to a `Clock` (`ErrKeyExpired`, also → 401 at the HTTP edge) so
  the reason is explicit for logs/tests while staying opaque on the wire.
- **`last_used_at` is stamped at most once per minute** (SPEC-09 §3): the
  verifier reads the current `last_used_at` and skips the write if it is within
  the throttle window, so a busy key is not one DB write per request. A failed
  stamp is swallowed — it is a convenience field, not an auth gate — so a stats
  write never blocks an authenticated call.
- **Scopes are exactly `query`, `ingest`, `admin`** (SPEC-07 §2, FR-ACC-04), a
  typed `Scope` with a closed `validScopes` set; `ParseScope`/`ParseScopes`
  reject anything else (and reject an empty set, so no capability-less key), so
  no invented scope can be minted or authenticate. `RequireScope(scope)`
  middleware authenticates the Bearer key, requires the scope (403 on a miss),
  and injects the key's tenant into the request context (FR-ACC-03 — the tenant
  is derived from the credential, never a client parameter).
- **Service + middleware only; router wiring deferred to STORY-04.1.**
  `APIKeyService` (Create/List/Revoke) and `APIKeyVerifier` / `RequireScope` are
  real, unit-tested with a fake DB and `net/http/httptest`, and proven end to end
  against the real control-plane Postgres. No full server is built; the deferral
  is recorded, not stubbed. The existing `api_keys` table already matches the
  spec, so no migration or `schemas/control_plane.sql` change was needed and the
  drift guard stays green.

## Consequences
- API keys are a self-contained control-plane capability the EPIC-04 router
  mounts with no change to `internal/tenant` (still the data-plane-only path,
  ADR-0003) and no tenant content in the control plane (C-3).
- The hex-prefix choice makes key parsing total and unambiguous; the format is
  frozen by this ADR, so a future encoding change is a versioned migration, not a
  silent edit.
- Revocation is immediate by construction (SQL filter) at the cost of one DB
  round trip per authentication — acceptable for the admin/API volume and
  consistent with session lookup.
- The pure helpers (secret minting/format, scope parsing, the verifier's branch
  logic and the service's validation via a fake DB) are unit-tested; the
  create → authenticate → list → revoke → expired → out-of-scope golden path is
  proven end to end against the real control-plane Postgres.
