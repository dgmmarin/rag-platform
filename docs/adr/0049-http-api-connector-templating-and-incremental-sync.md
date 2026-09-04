# ADR-0049: HTTP API connector — text/template mapping with helpers, JSONPath metadata, a connector_state cursor, and the incremental/weekly-full split

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-SRC-07, FR-SRC-08, SPEC-04 §4, C-1, C-3, C-4, NFR-MNT-01 · **Decisions:** ADR-0003, ADR-0008, ADR-0040, ADR-0046, ADR-0048

## Context

STORY-07.6 (ADR-0048) built the HTTP API connector's fetch/auth/pagination/rate-limit
**engine** and left exactly one seam — `buildDocument` — plus a placeholder Document
(raw item JSON as text). STORY-07.7 (FR-SRC-07/08, SPEC-04 §4) fills that seam with the
per-item **mapping** and adds **incremental** sync:

- render each record with a Go `text/template` (helpers `join`, `money`, `date`) to the
  document body, and `uri_template` to the citation URI;
- extract `metadata` via the 07.6 dot-path JSONPath evaluator, `id_path`→`ExternalID`,
  `updated_path`→`ModifiedAt`;
- fetch incrementally via `incremental_param` with a cursor persisted in `SyncRun.State`;
- support a weekly full sync for deletion detection.

The auth/pagination/egress/jsonpath code from 07.6 is untouched.

## Options and decisions

- **Templates are Go `text/template`, compiled once per endpoint.** SPEC-04 §4 names the
  engine (`text/template`) and helpers, so there is no library choice to make — the
  stdlib does it (lazy-senior rung 2). `newDocMapper` parses the `template` and
  `uri_template` once and reuses the `*template.Template` for every item across every
  page. A template **parse** error is a config problem (it fails every item identically),
  so it is caught at `ValidateConfig`/`Test` time (added to `validateSemantics`) and again
  fails `Sync` loudly; a template **execution** error is per-item — it is recorded and the
  item skipped, never aborting the whole sync (one bad record cannot sink a source).

- **Helpers are total (never return an error).** `join` (list + separator; accepts a
  decoded JSON `[]any`/`[]string`/nil), `money` (two-decimal amount from `json.Number`/
  number/numeric-string, optional currency suffix), and `date` (parse a timestamp string
  or epoch seconds, reformat with a Go layout; default RFC3339). Malformed input renders a
  predictable fallback (the value's `fmt` form, or empty) rather than erroring. Because no
  helper errors, an execution error can only come from field navigation (e.g. descending
  into a scalar), whose message carries only the author-supplied template path and Go
  type names — never item content — so skipped-item errors are safe to surface and the
  logger records only the endpoint + sequence number (SPEC-10: no content at info level).

- **Missing fields render `<no value>` (the `text/template` default; `missingkey` left at
  `invalid`).** SPEC-04 offers the choice explicitly. `missingkey=zero` is unhelpful for
  the `map[string]any` a decoded JSON object is (its element type is `interface{}`, whose
  zero renders as `<nil>`), and `missingkey=error` would drop a whole document for any
  missing optional field. The default is predictable, visible (a missing field is obvious
  in the body and in review, not a silent empty string), and requires no option. Template
  authors reference fields they know their API returns; a persistent `<no value>` is a
  signal to fix the template. Pinned by `TestMissingFieldRendersNoValue`.

- **Metadata reuses the 07.6 dot-path evaluator — no JSONPath dependency.** Each
  `metadata` entry `{key: "$.a.b"}` is evaluated with `evalPath` (jsonpath.go) and the raw
  resolved value stored in `Document.Metadata[key]`; an absent/null path is skipped. This
  is the reuse ADR-0048 anticipated.

- **The incremental cursor is persisted in a new generic `connector_state` table.** SPEC-04
  §1's `StateStore` needs a durable backing. The crawler used a **dedicated** `crawl_pages`
  table (ADR-0043); the API connector needs a **generic** per-source key/value store, and
  none existed. STORY-07.7 adds `connector_state (source_id uuid, key text, value text,
  updated_at timestamptz, primary key (source_id, key))` via tenant migration
  **00002_connector_state.sql**, plus `tenantStateStore` (statestore.go) reached only
  through `*tenant.DB` (ADR-0003, C-3). No `tenant_id` column (C-1); `source_id` is an
  informational copy of a control-plane id (SPEC-03 §2 invariant 4), no cross-DB FK. The
  store is generic (not API-specific) so a future connector can reuse it. `schemas/tenant.sql`
  is updated in the same change and the drift guard (`TestTenantSchemaMatchesMigrations`)
  and version guard (`ExpectedTenantVersion`→2) stay green.

- **Incremental mechanics: cursor via the endpoint Path, not a paginate.go change.** The
  cursor must ride on **every** page request, but 07.6's pagination is off-limits. On a
  non-full run with an `incremental_param` configured and a stored cursor, `Sync` bakes
  `?<incremental_param>=<cursor>` onto a COPY of the endpoint's `Path` before calling
  `enumerate`; `withQuery` (paginate.go) already preserves pre-existing query params on
  each page, so the cursor rides along untouched. As items stream, `Sync` tracks the max
  parsed `updated_at` and remembers the **verbatim** source value; after a successful
  enumeration it `Set`s that verbatim string back (so the next `updated_since` matches the
  format the API emits). The cursor advances on **both** full and incremental runs whenever
  `updated_path` is configured. A first incremental run (no cursor) or a full run sends no
  `incremental_param` and enumerates everything.

- **Full-vs-incremental is driven by `SyncRun.Full`; the weekly cadence is EPIC-09's.**
  `Full==true` ⇒ full enumeration (no cursor sent) and `sink.Complete` reconciles deletions
  (SPEC-04 §1). `Full==false` ⇒ incremental via the cursor; the sink no-ops `Complete` on
  an incremental run (SPEC-05 §5). The connector records `api:last_full_sync` in
  `connector_state` after a full run as a breadcrumb for the scheduler. It deliberately does
  **not** auto-promote an incremental run to full: deletion detection is the *sink's*
  `Complete`, and the sink's full/incremental mode is chosen by the worker from
  `SyncRun.Full` — a connector that flipped its own enumeration to full could not also flip
  the sink, so an auto-promotion would enumerate fully yet never reconcile deletions
  (misleading, half-effective). The weekly cadence that sets `SyncRun.Full` **and** builds
  a full-mode sink belongs with the EPIC-09 scheduler; 07.7 supplies both modes and the
  `last_full_sync` breadcrumb it needs. This mirrors the crawler's deletion-reconciliation
  split (ADR-0046).

## Consequences

- The API connector now emits fully-mapped documents (templated body, citation URI,
  metadata, modified time) and resumes incrementally across syncs. Adding template helpers
  or metadata paths needs no change outside this package (NFR-MNT-01).
- One new tenant migration (00002) and a generic `connector_state` table; the tenant schema
  version is now 2 and the resolver fails closed against tenants behind it. No control-plane
  change, no OpenAPI change, **no new dependency** (`text/template`, `time`, `strconv` are
  stdlib).
- Deletion detection for the API connector remains the periodic full re-enumeration the
  scheduler drives (EPIC-09), consistent with the crawler/sitemap connectors.
- Missing template fields surface as `<no value>`; documented and pinned by test, so a
  future change to `missingkey` is a deliberate, visible decision.
