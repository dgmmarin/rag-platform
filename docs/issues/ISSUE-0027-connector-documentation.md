# ISSUE-0027: Connector documentation

**Type:** Chore (docs) · **Status:** Done · **Story:** STORY-07.9 · **Traces:** SPEC-04 (all sections), SPEC-07 §2 (sources/documents API), SPEC-09 §2/§4 (security)

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). STORY-07.9 is the EPIC-07 documentation story (no FR trace); this
> issue records the docs-only change per the repo convention that every change
> carries an issue.

## Summary
Write tenant-admin-facing reference documentation for every connector kind, grounded
in the code as actually built across STORY-06.3 and STORY-07.1–07.8 (not the SPEC's
aspiration). Closes the AC "docs/connectors/*.md with config reference and examples
per kind" and completes EPIC-07 (39/39 pts).

## Scope
- **`docs/connectors/README.md`** — overview: what a connector is, the create → test
  → sync lifecycle, how credentials are supplied/secured (encrypted, never returned),
  the shared behaviours (SSRF egress guard, per-host politeness, 20 MB size cap /
  30 s timeout / ≤10 redirects, conditional fetch, rate-limit + `Retry-After`),
  test-connection semantics (≤10 s deadline, sanitised actionable errors), and the
  kinds table.
- **`docs/connectors/upload.md`** — not-scheduled push flow, `POST /v1/documents`,
  the `.pdf/.docx/.md/.html/.htm/.txt/.csv` allowlist with MIME sniffing, the
  `settings.limits.max_upload_mb` ceiling (default 50 MB) falling back to
  `MAX_UPLOAD_BYTES`, re-upload → new version. Config reference (none) + example.
- **`docs/connectors/web_crawl.md`** — full config reference with types/defaults/
  required-ness from `webcrawl.go`'s JSON Schema and `withDefaults` (start_urls req;
  defaults max_depth 3 / max_pages 1000 / delay_ms 0 / concurrency 4; render_js
  rejected), behaviour, ceilings, example.
- **`docs/connectors/sitemap.md`** — sitemap config reference, sitemap +
  sitemapindex + gzip, recursion ceilings (depth 5 / 200 docs / 50 000 URLs),
  `lastmod` incremental, no link following, shared crawler behaviours, example.
- **`docs/connectors/api.md`** — base_url; the 4 auth types with EXACT credential
  keys (`api_key`/`token`/`username`+`password`/`client_id`+`client_secret`) and the
  `header`/`token_url`/`scopes` config; endpoints[] fields; the 5 pagination types
  with their params/defaults; template helpers (`join`/`money`/`date`) and the
  `<no value>` missing-field behaviour; the minimal dot-path evaluator (NOT full
  JSONPath); incremental sync + the scheduler-owned weekly full sync; rate-limit /
  `Retry-After`; SSRF on base_url and token_url. Full worked example + credential
  examples per auth type.

## Out of scope
No connector code, schema, migration, or OpenAPI change. No ADR (pure docs; no new
documentation-structure decision was needed — the pages follow the existing house
style and the SPEC-04 §7 "docs page per kind" convention).

## Discrepancies noted (documented, not fixed in code)
Grounding the docs against the code surfaced two doc-vs-code gaps, recorded in the
pages as "Spec-vs-code note" rather than changed:

1. **web_crawl defaults vs SPEC example.** SPEC-04 §2's illustrative config shows
   `max_depth 5 / max_pages 5000 / delay_ms 500 / concurrency 8`; the built-in
   defaults (`withDefaults`) are `3 / 1000 / 0 / 4`. The example values are
   illustrative, not defaults. Documented in `web_crawl.md`.
2. **`settings.limits.max_pages_per_crawl` not wired.** The tenant setting exists
   (`settings_schema.json` / `settings_defaults.json`, default 5000) but the crawler
   does not consult it; the effective cap is the per-source `max_pages` (default
   1000). Documented in `web_crawl.md`. A follow-up could enforce the tenant ceiling
   in the connector; out of scope for this docs story.

Also documented (not a defect): the `api` connector takes all secrets from the
separate `credentials` object, never from `config` — the SPEC-04 §4 example implies
the secret inline; the realised behaviour and exact key names are documented in
`api.md`.

## Acceptance / DoD evidence
- **AC met:** one page per kind under `docs/connectors/`, each with a config
  reference AND a copy-pasteable example, plus an index (`README.md`). Every field,
  default, credential key, and limit stated is verifiable against the code/schemas:
  `internal/connector/webcrawl/{webcrawl,sitemapconn,sitemap}.go`,
  `internal/connector/upload/upload.go`,
  `internal/connector/api/{api,auth,paginate,jsonpath,mapping,egress}.go`,
  `internal/documents/{errors,handlers,sniff}.go`, `internal/egress/{egress,classify}.go`,
  `internal/config/config.go`, `internal/cp/tenants/settings_defaults.json`.
- **No code touched:** `go build ./...` passes; `git status` shows only additions
  under `docs/`.
- **Backlog updated:** STORY-07.9 → Done; EPIC-07 → 39/39 ✅ Complete
  (`BACKLOG_STATUS.md`, `BACKLOG_TASKS.md`).
