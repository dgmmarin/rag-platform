# ISSUE-0024: HTTP API connector — templating and incremental sync

**Type:** Feature · **Status:** Done · **Story:** STORY-07.7 · **Traces:** FR-SRC-07, FR-SRC-08, SPEC-04 §4/§4b, ADR-0049 (Decisions: ADR-0003, ADR-0008, ADR-0040, ADR-0046, ADR-0048)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.7 for traceability; the backlog story
> remains the authoritative work item. STORY-07.7 advances EPIC-07 to 35/39.

## Summary
Fill the 07.6 `buildDocument` seam with the per-item MAPPING and add INCREMENTAL sync
to the HTTP API connector (`internal/connector/api`, SPEC-04 §4, FR-SRC-07/08): render
each record with a Go `text/template` (helpers `join`/`money`/`date`) to the document
body and `uri_template` to the citation URI; extract `metadata` via the 07.6 dot-path
JSONPath evaluator; map `id_path`→`ExternalID` and `updated_path`→`ModifiedAt`; and
fetch incrementally via `incremental_param` with a cursor persisted in a new generic
`connector_state` tenant table.

## Scope
- **`internal/connector/api/mapping.go`** (new) — `docMapper` (templates parsed once per
  endpoint, reused per item); the `join`/`money`/`date` helper `FuncMap` (total funcs,
  predictable fallbacks); `build` producing the Document (body/uri/metadata/ExternalID/
  ModifiedAt) plus the verbatim + parsed `updated_at` for the cursor; shared timestamp
  parsing. Missing template fields render `<no value>` (the `text/template` default;
  `missingkey` left at `invalid`) — documented in ADR-0049 and pinned by test.
- **`internal/connector/api/api.go`** — `Sync` rewritten to use the mapper, record-and-skip
  a per-item template execution error (never abort the sync), track the max `updated_at`,
  read/write the incremental cursor, and record `api:last_full_sync`; `withIncrementalParam`
  bakes the cursor onto the endpoint Path (no paginate.go change); `validateSemantics` now
  rejects a malformed template/uri_template; state-key constants + `cursorKey`.
- **`internal/connector/api/statestore.go`** (new) — `tenantStateStore` /
  `NewTenantStateStore`: the generic `connector.StateStore` over `*tenant.DB`
  `connector_state` (ADR-0003, C-3; no `tenant_id`, no cross-DB FK). Wired as
  `SyncRun.State` by the worker (EPIC-09); unit tests use an in-memory StateStore.
- **`internal/migrate/tenant/00002_connector_state.sql`** (new) + **`schemas/tenant.sql`**
  — the generic per-source KV table; tenant schema version → 2.

## Out of scope
All-kinds live "test connection" (07.8); connector docs (07.9). The weekly-full-sync
CADENCE (the scheduler that sets `SyncRun.Full` and builds a full-mode sink) is EPIC-09;
07.7 supplies both modes + the `last_full_sync` breadcrumb (ADR-0049). No change to
07.6's auth/pagination/egress/jsonpath. No control-plane change, no OpenAPI change, no
new dependency (`text/template` is stdlib).

## Acceptance / DoD evidence
- **TDD:** tests written RED first (undefined `newDocMapper`/`cursorKey`/
  `stateLastFullSyncKey`), then GREEN. `mapping_test.go` — template golden mapping (body/
  uri/metadata/ExternalID/ModifiedAt), raw-JSON fallback, missing-id→seq,
  `<no value>` missing-field, execution-error-returned, parse-error-rejected, each helper
  (`join`/`money`/`date` incl. verbatim fallbacks), `updated_path` parsing.
  `incremental_test.go` — cursor round trip (run 1 sends no `updated_since` and stores the
  max; run 2 sends the cursor and sees only the newer item, then advances), full-run
  ignores the cursor + calls `Complete` + records `last_full_sync`, nil-State degrades to
  full enumeration. Existing `TestSyncAuthPaginationMatrix` (no template ⇒ raw-JSON
  fallback) stays green.
- **e2e / golden path:** `test/e2e/api_e2e_test.go` (`//go:build e2e`,
  `TestAPIConnectorIncrementalCursorPersists`) against a REAL enrolled tenant DB via the
  resolver + `*tenant.DB`: template/uri/metadata mapping over a live paginated httptest
  API, and the incremental cursor persisted to and read back from `connector_state` across
  two runs (asserted over `pgxpool`, never `docker compose exec`). PASS (5.54s).
- **mise tasks:** `mise run up` (stack healthy) → `mise run e2e` path green for the new
  test; `internal/connector/api` unit tests green, coverage **79.3%** (≥ 70 % connector
  gate); lint clean (golangci-lint v2.13.1, 0 issues); tenant drift guard
  (`TestTenantSchemaMatchesMigrations`) and version guard (`ExpectedTenantVersion`→2) green.
- **Constraints:** cursor persisted only via `*tenant.DB` (ADR-0003, C-3); no `tenant_id`,
  no cross-DB FK (C-1, SPEC-03 §2 inv. 4); no tenant content in the control plane (C-3);
  template execution errors sanitised (no item content in errors/logs); Go only (C-2). No
  control-plane/OpenAPI change; one additive tenant migration; no new dependency.
