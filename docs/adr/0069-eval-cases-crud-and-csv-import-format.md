# ADR-0069: Eval cases — a tenant-DB CRUD store, `ragctl eval` grammar, and the CSV import format

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-04 · **Decisions:** ADR-0003, ADR-0009

## Context
FR-ADM-04: the platform exposes a per-tenant evaluation harness — question/expected-answer
pairs that can be run to measure retrieval and answer quality. The `eval_cases`,
`eval_runs` and `eval_results` tables already exist in the tenant schema
(schemas/tenant.sql). STORY-12.1 delivers the **authoring** side only: CRUD over
`eval_cases` plus a CSV import. Running the cases (`ragctl eval run`, recall@k / grounded
rate / latency, writing `eval_runs`/`eval_results`) is STORY-12.2 and is out of scope here.

Eval cases are tenant content (C-3): they must be reached only through a `*tenant.DB` from
the resolver (ADR-0003), never the control plane, and there is no `tenant_id` column — the
database boundary is the tenant boundary (C-1).

The one real interface decision is the **CSV column format** an operator authors and
imports; the rest mirrors existing patterns (the documents store/service, the Kong CLI
grammar).

## Options / decisions
- **A store/service package `internal/eval`, reached through the resolver.** `eval.Store`
  takes a `*tenant.DB` on every method (Create/Get/List/Update/Delete/Import), exactly like
  `documents.Store` (ADR-0003, ADR-0030): the SQL is exercised by the e2e suite because a
  `*tenant.DB` is unforgeable, while the pure struct mapping (`scanCase`), input validation
  and CSV parsing are unit-tested directly. `eval.Service` owns the resolver and maps
  resolver lifecycle outcomes to `ErrTenantUnavailable`, mirroring `documents.Service`.

- **`ragctl eval <add|list|edit|rm|import>` — a Kong subcommand group (ADR-0009).** Each
  takes a tenant `--slug`; the command opens the control-plane pool, loads the startup DEK,
  builds the resolver, and looks the tenant id up from its slug (`select id from tenants
  where slug = $1`). This is the same slug→tenant resolution the lifecycle commands use, and
  the CLI is the entry point exactly as enroll/suspend/delete are for their operations. The
  HTTP/admin-UI surface is STORY-12.4, not this story. `edit` is a get-then-merge: only the
  flags passed change.

- **CSV format (the load-bearing decision).** A header row is required. Recognised columns,
  matched case-insensitively and trimmed, are:
  `id`, `question`, `expected_answer`, `expected_doc_ids`, `tags`. `question` is the only
  required column; column order is free; unknown columns are rejected (fail closed, not
  silently ignored). Each remaining column is optional.
  - **Multi-valued cells (`expected_doc_ids`, `tags`) use `|` as the item separator**, items
    trimmed and empty items dropped. `|` was chosen over comma (which would force the whole
    cell to be quoted and, worse, a spreadsheet would split it into columns) and over
    whitespace (so a tag may contain spaces). The cell itself is still standard RFC 4180:
    encoding/csv quotes a cell containing commas, quotes or newlines.
  - **`id` is the upsert key.** A row with a non-empty, well-formed `id` upserts (insert with
    that id, or update the existing case); a row with no `id` inserts a fresh case with a
    generated id. This makes an export→edit→re-import round trip stable and lets an operator
    re-run an import idempotently.
  - **Validation is the trust boundary and fails closed on the first problem**, naming the
    offending column, 1-based row (header = row 1) and value: empty `question`, malformed
    UUID in `id` or `expected_doc_ids`, unknown/missing headers. The whole file is parsed and
    validated before anything is written.

- **Import is atomic.** `Store.Import` applies all rows in one tenant-DB transaction, so a
  failure part-way writes nothing — an import is all-or-nothing. Upsert-vs-insert is counted
  from the `xmax = 0` sentinel on the `INSERT … ON CONFLICT … RETURNING` row.

## Consequences
- A tenant's eval set can be authored incrementally (`add`/`edit`/`rm`) or in bulk (a CSV an
  operator can keep under version control), with the CSV format documented and stable for
  STORY-12.2 and the STORY-12.4 admin UI to build on.
- No schema change: the existing `eval_cases` columns are used as-is; `expected_doc_ids` are
  informational UUIDs (Invariant 4, no cross-DB FK) — they are validated as UUIDs but not
  checked for existence, matching how the harness will compute recall@k in STORY-12.2.
- Adding the harness required no change outside `internal/eval` plus its one-line CLI
  registration, consistent with the platform's extension conventions.
- **ponytail:** `List` returns the newest N cases (default cap 1000) with no keyset cursor —
  eval sets are small, human-authored. Ceiling: a tenant with more than the cap sees only the
  newest. Upgrade path: add a keyset cursor like `internal/documents` if that ever bites.
- **ponytail:** the CLI `edit` cannot clear a list back to empty (a supplied `--tag`/`--doc-id`
  replaces the list; an absent one preserves it). Upgrade path: add `--clear-tags` /
  `--clear-doc-ids` flags if needed. Import (full replace via upsert) has no such limit.
