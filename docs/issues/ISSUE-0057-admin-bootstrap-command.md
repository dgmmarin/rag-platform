# ISSUE-0057: `ragctl admin bootstrap` + dev seed (STORY-11.1 Task 6)

**Type:** Feature · **Status:** Done · **Story:** STORY-11.1 (support task) · **Traces:** FR-ADM-07, FR-ACC, SPEC-02 §4

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0074).

## Summary
STORY-11.1's admin UI (shell + `/v1/auth/me`, Tasks 1–5) had no way to log in: signup only creates a
bare, tenant-less, non-admin user, and nothing seeds the database. This issue adds the missing
operational path — a `ragctl` command to create or promote a platform-admin user, and a dev-seed
task that uses it (plus the existing `enroll`) to populate a demoable local stack in one command.
STORY-11.1 itself is not marked done by this issue (that is a later task).

## Scope (shipped)
- **`internal/cp/auth/bootstrap.go`** — `Service.BootstrapAdmin(ctx, email, password string,
  platformAdmin bool) (created bool, err error)`: create-or-promote via a single race-safe `insert
  ... on conflict (email) do update set is_platform_admin = excluded.is_platform_admin returning
  (xmax = 0)`. Reuses `normalizeEmail`, `minPasswordLen`, `HashPassword` from `Signup`. Never touches
  an existing user's password.
- **`internal/cli/admin.go`** — `AdminCmd` / `AdminBootstrapCmd`, registered in `internal/cli/cli.go`
  as `ragctl admin bootstrap --email <email> [--platform-admin]` (default true, negatable). Fails
  closed on a missing control-plane URL before any DB dial. Reads the password from
  `RAGCTL_ADMIN_PASSWORD`, else one line from stdin (TTY prompt on stderr only when interactive) —
  never from argv. Builds `auth.Service` from the control-plane pool the same way `api_server.go`
  does. Prints only non-secret confirmation (email, created-vs-promoted, `platform_admin`).
- **`mise-tasks/seed`** — runs `ragctl admin bootstrap` for a demo admin (`admin@example.com`,
  `RAGCTL_ADMIN_PASSWORD` with dev fallback `devpassword123`) and `ragctl enroll` for two demo
  tenants (`demo`, `acme`); idempotent (both underlying commands already are); self-skips (exit 0)
  without `CONTROL_PLANE_URL`, mirroring `mise-tasks/backup-drill`/`eval-gate`; prints the demo
  credentials on success.

## Decisions (ADR-0074)
- Password via env/stdin, never argv (ps/shell-history leak, SPEC-09 §2/§3).
- Create-or-promote in one upsert (race-safe, mirrors the `internal/eval/store.go` `xmax = 0`
  pattern); on-conflict never writes `password_hash`.
- Platform-admin is a user flag (`is_platform_admin`), not a tenant membership — no membership row
  is created here.

## Tests / runnable checks
- **Unit** (`internal/cp/auth`): `TestBootstrapAdminCreatesNewUser` (created=true, hash verifies,
  `is_platform_admin` written true), `TestBootstrapAdminPromotesExistingUser` (created=false,
  on-conflict SQL never mentions `password_hash`), `TestBootstrapAdminRejectsShortPasswordBeforeWrite`,
  `TestBootstrapAdminRejectsEmptyEmail` (both reject before any DB call). RED (method undefined) →
  GREEN captured; `go test ./internal/cp/auth/`: **PASS**.
- **CLI** (`internal/cli`): `TestAdminBootstrapRequiresURL` — `admin bootstrap --email x@y.z` with
  `RAGCTL_ADMIN_PASSWORD` set but no control-plane URL fails closed mentioning "control-plane URL",
  never `ErrNotImplemented`. RED (unknown subcommand before wiring) → GREEN captured; `go test
  ./internal/cli/`: **PASS**.
- **Build/lint**: `mise run build` PASS; `mise run lint` shows 0 issues in new/changed files, total
  stays at the 10 pre-existing baseline.
- **Seed script**: `bash -n mise-tasks/seed` OK; demonstrated self-skip and/or a live run against the
  local stack (see the task report for the exact transcript).

## Not in scope here
- STORY-11.1's own Definition of Done / marking the story complete (Task 8).
- A `--reset-password` flag for bootstrap (nothing calls for it yet — YAGNI; noted as a future
  extension point in ADR-0074).
- Seeding ingested documents/chunks (needs the ingestion pipeline + provider keys) — the seed task
  only creates the admin user and demo tenants.
