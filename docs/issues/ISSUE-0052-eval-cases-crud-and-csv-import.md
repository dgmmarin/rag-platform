# ISSUE-0052: Eval cases CRUD and CSV import (STORY-12.1)

**Type:** Feature · **Status:** Done · **Story:** STORY-12.1 · **Traces:** FR-ADM-04

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0069).

## Summary
Deliver the authoring side of the per-tenant evaluation harness (FR-ADM-04): CRUD over the
existing tenant `eval_cases` table plus a CSV import, driven by a new `ragctl eval`
subcommand group. Running the cases (recall@k, grounded rate, latency) is STORY-12.2 and is
NOT built here; the admin-UI eval report is STORY-12.4.

## Scope
- **`internal/eval`** — a new package reached through the resolver (ADR-0003, C-3):
  - `Case` / `CaseInput` / `ImportResult` types over `eval_cases` (no `tenant_id`; the
    database boundary is the tenant boundary, C-1).
  - `Store` (interface) + `TenantStore`: Create / Get / List / Update / Delete and an atomic
    `Import` (one tenant-DB transaction; a row with an `id` upserts, one without inserts).
  - `Service`: owns the resolver, validates input at the trust boundary, maps resolver
    lifecycle outcomes to `ErrTenantUnavailable`, mirrors `documents.Service`.
  - `ParseCSV`: the CSV trust boundary — required `question` header, `|`-separated
    `expected_doc_ids`/`tags`, optional `id` upsert key, fail-closed validation naming the
    offending row/column/value (format in ADR-0069).
- **`internal/cli/eval.go`** — `ragctl eval add|list|edit|rm|import`, each taking a tenant
  `--slug`, resolving slug→tenant id from the control plane and building the resolver. One
  line registers `EvalCmd` in `internal/cli/cli.go`.

## Tests / runnable checks
- **Unit** (`internal/eval`): `ParseCSV` happy path, question-only, upsert `id` column,
  missing/unknown header, empty question, malformed UUID (`expected_doc_ids` and `id`), empty
  input, header-only, case-insensitive headers; `splitList`; `CaseInput.validate`; `scanCase`
  struct mapping (incl. NULL answer) via a fake row seam. `mise run test`: **PASS**.
- **CLI** (`internal/cli`): `TestEvalCommandsRequireURL` — add/list/edit/rm fail closed with a
  clear "control-plane URL" error when none is set (hermetic via the package `TestMain`).
- **e2e** (`test/e2e/eval_e2e_test.go`, `//go:build e2e`): golden path against a REAL enrolled
  tenant DB via the resolver — Create/Get/Update/List round-trip the `uuid[]`/`text[]`
  columns, CSV import creates + upserts by id in one transaction, Delete is idempotent, and
  `ragctl eval import` drives the same path end to end. `go test -tags e2e -run
  TestEvalCasesGoldenPath`: **PASS** (4.9s).
- **Build**: `mise run build`: **PASS**. **Lint**: `mise run lint` reports zero issues in the
  files added/changed by this story.

## Not in scope
- `ragctl eval run` and the recall@k / grounded / latency metrics (`eval_runs`,
  `eval_results`) — STORY-12.2.
- LLM-as-judge scoring — STORY-12.3.
- Admin-UI eval report and CI gate — STORY-12.4.
- The 10 pre-existing lint issues on unrelated files (`connector/connector.go`,
  `cp/audit/pool.go`, `ingest/parse/*`, `connector/webcrawl/egress_test.go`,
  `egress/egress_test.go`, `test/e2e/{audit,retrieve_endpoint}_e2e_test.go`) — untouched.
