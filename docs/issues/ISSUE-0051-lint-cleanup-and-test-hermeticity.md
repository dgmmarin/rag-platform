# ISSUE-0051: Lint cleanup in EPIC-09/10 code and cli test hermeticity

**Type:** Chore · **Status:** Done · **Story:** — (follow-up to EPIC-09/EPIC-10) · **Traces:** NFR-MNT-03

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
The uncommitted EPIC-09/EPIC-10 work was marked "lint clean" in the backlog, but `mise run lint`
reported 22 issues and `mise run test` failed locally. This clears every lint issue introduced by
that work and makes the `internal/cli` unit tests hermetic. No behaviour changes: all fixes are
style/quality or test-only.

## Scope
- **Test hermeticity** (`internal/cli/cli_test.go`): the `*RequiresURL` tests assert a command
  fails closed when no connection URL is set, but kong resolves `CONTROL_PLANE_URL` /
  `PROVISION_DB_URL` from the environment (env tags). A developer shell exporting them (the local
  stack does) masked the precondition, so the commands dialled a default instead of erroring.
  Added a package `TestMain` that unsets those two vars for the whole test binary. CI was unaffected
  (clean env); this only fixed local runs.
- **errcheck** on `defer tx.Rollback(ctx)` → `defer func() { _ = tx.Rollback(ctx) }()` in
  `internal/cp/sources/pool.go`, `internal/documents/delete_source.go`,
  `internal/documents/jobs.go`, `internal/worker/scheduler.go` (post-commit rollback is
  intentionally ignored).
- **unused** dead `stub` helper removed from `internal/cli/commands.go` (plus its orphaned `io`
  import); `ErrNotImplemented` stays (defined in `cli.go`, used by tests).
- **redefines-builtin** `cap`/`max` locals renamed: `internal/worker/limiter.go` (`cap` param →
  `capacity`), `internal/worker/limiter_test.go` (`cap` const → `capN`), `internal/worker/mirror_test.go`
  (`max` param → `maxAttempts`).
- **staticcheck SA4000** `internal/worker/limiter_test.go`: split `!acquire("A") || !acquire("A")`
  (two intentional stateful calls) into two separate `if`s — same behaviour, no longer flagged.
- **unused-parameter** renamed to `_` in `internal/obs/middleware_span_test.go` and
  `internal/worker/mirror_test.go`.

## Tests / runnable checks
- `mise run build`: **PASS**.
- `go test ./...` with the local stack env exported: **PASS** (previously `internal/cli` failed).
- `mise run lint`: EPIC-09/10 files (`cli`, `worker`, `obs`, `documents`, `cp/sources`) now report
  **zero** issues.

## Not in scope
- 10 pre-existing lint issues on files untouched by EPIC-09/10 (`connector/connector.go`,
  `cp/audit/pool.go`, `ingest/parse/{markdown,parse}.go`, `connector/webcrawl/egress_test.go`,
  `egress/egress_test.go`, `test/e2e/{audit,retrieve_endpoint}_e2e_test.go`). These predate this
  work on `HEAD`; a separate cleanup issue should address them.
- e2e run (`mise run e2e`) — requires the live stack, not exercised here.
