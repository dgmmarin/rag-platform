# ISSUE-0069: Eval report render in the admin UI (STORY-12.4 carry-in)

**Type:** Feature · **Status:** Done · **Story:** STORY-12.4 · **Traces:** FR-ADM-04, SPEC-06 §8, ADR-0072, ADR-0075

> Note: the eval data contract + CI gate shipped in ISSUE-0055. This issue is the deferred
> admin-UI **render** half of STORY-12.4, delivered under EPIC-11.

## Summary
STORY-12.4's report data (`ragctl eval report` / `eval run --json`) shipped in ISSUE-0055 but the
admin-UI render was deferred until the admin UI existed. This delivers it: a runs list and a per-run
drill-down over the eval report contract. The data lived only in the tenant DB (reached via the
resolver) and was CLI-only — no HTTP surface existed — so this adds two read-only session routes plus
a `ListRuns` read (the store previously had only `GetRun` by id).

## Scope
- **Task 1 — ListRuns read** (`internal/eval/report.go` + `service.go`): add `ReportReader.ListRuns`
  + `RunStore.ListRuns` (`select … from eval_runs order by started_at desc limit`) and
  `Service.ListRuns`. The per-run `Report` (`GetRun` + `Results`) already existed.
- **Task 2 — HTTP handlers** (`internal/eval/handlers.go`): `Handlers{Service}` with `RunList`
  (`GET .../eval/runs?limit`) and `Report` (`GET .../eval/runs/{id}`), reading the tenant from
  context (FR-ACC-03) and mapping `ErrNotFound`→404 / `ErrTenantUnavailable`→503 into the SPEC-07 §1
  envelope. Read-only — the mutating eval surface (cases CRUD, run) stays on the CLI.
- **Task 3 — router + wiring** (`internal/api/router.go`, `internal/cli/api_server.go`): Deps fields
  `EvalRunList`/`EvalReport`; mount `GET /admin/tenants/{tenantId}/eval/runs` and `.../eval/runs/{id}`
  behind `RequireSession -> RequireTenantAccess` on `RequireTenantSourcesRead`, no CSRF (reads). Wire
  the eval service (same construction as the CLI: resolver + `NewTenantStore` + `NewRunStore`).
- **Task 4 — UI** (`web/`): `web/lib/eval.ts` client + `useEvalRuns`/`useEvalReport`; `EvalRunsTable`
  (started, cases, recall@k, grounded, correctness, mean latency; row link) and `EvalReport` (run
  header, summary tiles, per-case results with recall/correctness tri-state marks). Pages
  `web/app/admin/eval` (list) + `web/app/admin/eval/[id]` (report) shadow the `[section]` placeholder.

## Out of scope (later work)
- Runs-list pagination (the store caps at `defaultRunListLimit`, no cursor yet).
- Triggering a run or editing cases from the UI — those stay on `ragctl eval`.
- Rendering the raw `config`/`summary` JSON blobs; the UI renders the known summary keys.

## Tests / runnable checks
- **`internal/eval/handlers_test.go`**: `TestParseLimit`, `TestWriteServiceErrorMapping`
  (404/503/500 + codes), `TestReportNoTenant` (401, no panic). `internal/eval` existing report/run
  tests unchanged. `go test ./internal/eval/`: **PASS**.
- **`internal/api/router_test.go`**: `TestTenantEvalRoutesChain` (both routes: session → read gate
  order, handler reached). `go test ./internal/api/`: **PASS**; `go build ./...`: **PASS**.
- **web (`web/`, vitest + Testing Library)**: `EvalRunsTable.test.tsx` (metrics as %, no-summary em
  dashes, row link, empty state) + `EvalReport.test.tsx` (summary tiles, per-case rows, deleted-case
  id fallback, no-summary / no-results notes). `cd web && npx vitest run`: **PASS** (96, +9);
  `npm run build`: clean, routes `/admin/eval` + `/admin/eval/[id]` present; `npm run lint`: clean.
