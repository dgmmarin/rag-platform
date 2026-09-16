# ISSUE-0061: Source form submits array/object config keys as scalar strings

**Type:** Bug · **Status:** Done · **Story:** STORY-11.2 · **Traces:** FR-ADM-01, SPEC-11 §10, SPEC-04 §2/§4

## Symptom
Creating a web-crawl source from the admin UI failed with:
```
connector config invalid: invalid config: start_urls: got string, want array
```

## Root cause
The connector-kind form schema (`FieldSpec.Type`, ADR-0075/STORY-11.2 Task 2) had only scalar input
types (`text|url|number|secret|bool`). Several config keys are NOT scalars:
- `start_urls` (web_crawl) and `sitemap_urls` (sitemap) are JSON **arrays of strings**.
- `auth` (object) and `endpoints` (array of objects) on the `api` connector are JSON **composites**.

All four were declared `Type:"text"`, so the schema-driven `SourceForm` rendered a single text input
and submitted the value as a **string**. Each connector's `ValidateConfig` (a JSON-Schema check)
then rejected it — `got string, want array`. The form could never produce a valid web-crawl,
sitemap, or api source.

## Fix
Extend the form-schema type contract with two composite types and render them correctly.
- **`internal/connector/connector.go`** — document `FieldSpec.Type` values `"stringlist"` (JSON array
  of strings) and `"json"` (arbitrary JSON object/array) alongside the scalars.
- **`webcrawl.go`** `start_urls` and **`sitemapconn.go`** `sitemap_urls` → `Type:"stringlist"`.
- **`api.go`** `auth` and `endpoints` → `Type:"json"`.
- **`web/lib/sources.ts`** — `ConnectorFieldType` adds `"stringlist" | "json"`.
- **`web/components/SourceForm.tsx`** — a `stringlist` renders a textarea (one entry per line) and
  submits a trimmed string **array**; a `json` renders a textarea and submits the **parsed** value,
  blocking submit with an inline "Enter valid JSON" message when it does not parse. Type-aware
  pre-fill on edit (array joined by newlines; object pretty-printed).

## Regression guard
- **`internal/connector/kinds_test.go`** — `TestConnectorKindsDriftGuard` gains a **type-drift**
  check: for every required key in each connector's baseline config, a value whose Go kind is a
  slice/map must NOT carry a scalar `FieldSpec.Type`. This fails loudly if a future connector
  declares an array/object key as `text` (the exact shape of this bug). `go test
  ./internal/connector/...`: **PASS**.
- **`web/components/SourceForm.test.tsx`** — a `stringlist`+`json` create submits
  `config.start_urls` as an array and `config.auth` as a parsed object; an unparseable `json` field
  blocks submit. `cd web && npx vitest run`: **PASS** (45/45, +2); `npm run build`: clean.
