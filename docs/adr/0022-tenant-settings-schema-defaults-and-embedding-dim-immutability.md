# ADR-0022: Tenant settings — embedded JSON Schema, a defaults overlay, and the flat `embedding_dim` mirror for immutability

**Status:** Accepted · **Date:** 2026-08-22 · **Requirements:** FR-TEN-08, SPEC-02 §5, SPEC-02 §6, SPEC-01 §6/§7, SPEC-03 §5, ADR-0015, C-3

## Context
STORY-03.5 adds tenant settings to the control-plane tenant surface
(`internal/cp/tenants`, SPEC-02 §2): `GET/PATCH /v1/settings` over the
`tenants.settings` JSON document, validated against a JSON Schema, with
`embedding.dim` immutable after provisioning and every change audited. The public
router lands in STORY-04.1, so — mirroring STORY-03.1/03.3/03.4 (ADR-0019/0021) —
the service and `http.Handler`s are built and tested (unit + real-Postgres e2e)
with route wiring deferred.

Two facts about the existing system had to be reconciled:

1. **The settings document key for the embedding dimension differs between
   specs.** SPEC-02 §5 documents the settings document with a *nested*
   `embedding.dim`. But SPEC-01 §6/§7 and ADR-0015 (the tenant-migration runner)
   read a *flat* top-level `tenants.settings.embedding_dim` to substitute the
   `vector(N)` dimension, and the provisioner (`internal/provision`) writes
   exactly that flat key at enrollment (`jsonb_build_object('embedding_dim', N)`).
   A freshly provisioned tenant therefore has `settings = {"embedding_dim": N}` —
   not a full SPEC-02 §5 document.

2. **A provisioned tenant's stored document is a stub.** Validating that stub
   against the full SPEC-02 §5 schema (which requires `embedding.provider/model`,
   `retrieval`, `limits`, …) would reject every first patch, because the stored
   document is incomplete.

Open sub-decisions this ADR records:
1. Where the schema lives and how per-field errors are produced.
2. How the two embedding-dimension keys are reconciled without touching the
   provisioning/migration code paths (which are load-bearing for EPIC-02).
3. How a stub document is made complete so validation and partial updates work.
4. How immutability is enforced and how changes are audited.

## Decision
- **The JSON Schema is an embedded repo asset.** `settings_schema.json` (a
  2020-12 schema realising SPEC-02 §5 field-for-field) is `//go:embed`-ed and
  compiled once at package init with `github.com/santhosh-tekuri/jsonschema/v6`
  (a maintained validator; no schema validator was previously pinned). A malformed
  asset panics at startup — loud and once — never per request. Validation walks the
  library's `ValidationError` cause tree to emit one `FieldError{field, message}`
  per leaf violation, keyed by dotted instance path (e.g. `retrieval.min_score`),
  which the HTTP layer returns as a `400` with a `fields` array.
- **The flat `tenants.settings.embedding_dim` remains the durable source of truth
  for the immutable dimension; the SPEC-02 §5 nested `embedding.dim` is a derived
  view.** The settings service does *not* change the provisioner or the migration
  runner (SPEC-01 §6/§7, ADR-0015): those keep reading/writing the flat key. On
  read, the service projects the flat value into nested `embedding.dim`; on write,
  it keeps the flat mirror equal to the provisioned dimension and never exposes the
  flat key in the API document. This is the least-blast-radius reconciliation of
  the SPEC-01/SPEC-02 wording — both specs remain accurate about the key each one
  owns, and EPIC-02 code is untouched, so the drift guard stays green (no schema
  change was needed; `audit_log` and `tenants.settings` already exist).
- **A defaults document overlays the stored stub.** `settings_defaults.json` (the
  canonical SPEC-02 §5 document) is embedded; the effective document a caller
  GETs/PATCHes is `defaults → stored → patch` (deep-merged), with `embedding.dim`
  then pinned to the provisioned dimension. This makes a freshly provisioned
  tenant present a complete, schema-valid document and makes a single-field PATCH
  succeed. The persisted document is the merged result plus the flat mirror, so a
  tenant's stored settings become self-describing after the first write.
- **`embedding.dim` immutability is enforced in the service, not the schema.** The
  schema constrains `embedding.dim`'s *shape* (positive integer); the service
  rejects a patch whose merged `embedding.dim` differs from the provisioned
  dimension with `ErrImmutableField` (→ `409 Conflict`), pointing the caller at
  the reindex job (SPEC-03 §5). Re-sending the same dimension is allowed, so a
  full-document PUT-style patch that echoes `embedding.dim` is not spuriously
  rejected. Everything else under `embedding` (provider, model) is mutable.
- **Every change is audited; nothing is written on rejection.** A successful PATCH
  writes one `settings.update` row to `audit_log` (SPEC-02 §6) recording the
  tenant, the actor (session user), the target, and the *set of changed top-level
  keys* — never the values, so no configuration content is duplicated into the
  audit log. Validation and immutability failures short-circuit before any
  `tenants` update or audit write.
- **Tenant comes from context, service + handlers only.** Both handlers read the
  resolved tenant from the request context (FR-ACC-03), never from the body or a
  query parameter, and the actor from the session. `SettingsService` and
  `SettingsHandlers` are unit-tested with a fake DB and `net/http/httptest` and
  proven end to end against the real control-plane Postgres; STORY-04.1 mounts the
  handlers behind `RequireSession` + `RequireRole(PermChangeSettings)` for PATCH.

## Consequences
- The SPEC-01 (flat `embedding_dim`) and SPEC-02 (nested `embedding.dim`) wordings
  are reconciled without renumbering or editing either spec's owned key: SPEC-02 §5
  gains a note that `embedding.dim` is the API view of the flat mirror and is
  immutable after provisioning. Should we later want a single key, that is a
  deliberate migration (rewrite provisioner + runner + stored rows), not a silent
  edit — captured here so the divergence is intentional, not accidental.
- Defaults live in one asset, so the "what a new tenant sees" contract is
  reviewable and testable; changing a platform default is an edit to
  `settings_defaults.json`, and the init-time check asserts the defaults satisfy
  the schema.
- Immutability is guaranteed for the value the data plane actually reads (the flat
  mirror), so a settings edit can never desync the stored dimension from the
  physical `vector(N)` column; changing the dimension remains a reindex/table swap
  (SPEC-03 §5).
- Adding the settings capability required no migration and no change to
  `internal/tenant` or `internal/provision`, keeping the data-plane path
  (ADR-0003) and provisioning untouched; the audit write is a direct
  `insert into audit_log` that STORY-03.6's audit package will later wrap.
