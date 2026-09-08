# ADR-0074: `ragctl admin bootstrap` (create-or-promote platform admin), password via env/stdin, `mise run seed`

**Status:** Accepted · **Date:** 2026-09-08 · **Requirements:** FR-ADM-07, FR-ACC, SPEC-02 §4 · **Decisions:** ADR-0009, ADR-0073, SPEC-09 §2/§3

## Context
EPIC-11's admin UI (STORY-11.1) now has a shell, a session-authenticated `GET /v1/auth/me`, and
login page — but no operator can actually log in: nothing seeds a user, and `POST /v1/auth/signup`
only ever creates a bare, tenant-less, non-admin user (`internal/cp/auth.Service.Signup`). There is
no CLI path to grant `is_platform_admin`. This blocks the whole story on "no login credentials" and
leaves the UI an empty shell with nothing to browse. This ADR adds the missing operational path: a
`ragctl` command to create or promote a platform-admin user, and a dev-seed task that uses it plus
the existing `enroll` command to populate a demo environment in one step.

## Options / decisions

- **`ragctl admin bootstrap --email <email> [--platform-admin]`, backed by `Service.BootstrapAdmin`
  in `internal/cp/auth`.** Reuses `Signup`'s building blocks (`normalizeEmail`, `minPasswordLen`,
  `HashPassword`) rather than duplicating validation/hashing — the two paths cannot drift. Rejected:
  a one-off script outside `ragctl` (SPEC-02 §7 already makes `ragctl` the single operational
  binary; a second script would duplicate connection/config resolution).

- **Password comes from `RAGCTL_ADMIN_PASSWORD`, or stdin when that's unset — never argv.** A CLI
  flag would leak the password via `ps` and shell history (SPEC-09 §2/§3 treat secrets as
  confidential, never logged or exposed on the process table). Precedence: env var first (scripted/
  CI use), else one line read from stdin, with a `password: ` prompt written to stderr only when
  stdin is a real TTY (so a piped/redirected invocation gets no prompt noise on stdout, which would
  otherwise corrupt scripted parsing). The same `minPasswordLen` floor `Signup` enforces applies
  here, checked before any DB write.
  - **ponytail:** the TTY path does not suppress keystroke echo (no `golang.org/x/term` dependency
    added for it). Ceiling: shoulder-surfing at an interactive prompt. Upgrade path: add `x/term` and
    switch to `term.ReadPassword` if that becomes worth a new dependency — the env-var and piped-
    stdin paths (the ones CI and `mise run seed` actually use) are unaffected either way.

- **Create-or-promote, idempotent, in one race-safe write.** `BootstrapAdmin` runs a single
  `insert into users (email, password_hash, is_platform_admin) values (...) on conflict (email) do
  update set is_platform_admin = excluded.is_platform_admin returning (xmax = 0)` — the same
  `xmax = 0`-detects-insert trick `internal/eval/store.go`'s case upsert already uses. This is
  race-safe (a concurrent bootstrap of the same email cannot interleave a lost update, unlike a
  select-then-branch) and reports whether the row was freshly inserted (`created`) or an existing
  row was promoted. **The on-conflict branch never touches `password_hash`**: re-running bootstrap
  against an existing operator email cannot silently reset their password. A deliberate password
  reset is out of scope here (left for a future explicit `--reset-password` flag, not built because
  nothing calls for it yet — YAGNI).

- **Platform-admin is a user flag, not a tenant membership.** `is_platform_admin=true` alone grants
  the cross-tenant admin surface (`GET /admin/tenants`, SPEC-02 §4); the tenant switcher (ADR-0073)
  lists every tenant for such a user regardless of `tenant_members`. Bootstrap therefore creates no
  membership row — a platform admin does not "belong" to a tenant, and inventing one would be either
  meaningless (which tenant?) or misleading (implying scoped, not cross-tenant, access).

- **Fail-closed control-plane URL check runs before any DB dial**, mirroring every other `ragctl`
  command that needs one (`migrate control`, `enroll`, `tenant *`): `--control-plane-url` /
  `CONTROL_PLANE_URL` unset is an actionable error mentioning "control-plane URL", never a stub
  response and never a driver-level connection error.

- **`mise-tasks/seed`: compose `admin bootstrap` + `enroll` into one demoable dev step.** An empty,
  credential-less stack is not useful to open the admin UI against. `seed` runs `ragctl admin
  bootstrap` for a fixed demo email (`admin@example.com`, password from `RAGCTL_ADMIN_PASSWORD` with
  a dev-only fallback `devpassword123`) and `ragctl enroll` for two demo tenants (`demo`, `acme`) so
  the tenant switcher is non-empty. Both underlying commands are already idempotent (bootstrap:
  create-or-promote; `enroll`/`Provisioner.Provision`: re-running an existing slug reuses/re-applies
  rather than erroring), so `seed` needs no extra guard to be safely re-run. It self-skips (exit 0,
  clear message) when `CONTROL_PLANE_URL` is absent, mirroring `mise-tasks/backup-drill` /
  `mise-tasks/eval-gate` — a keyless/stack-less environment is a clean no-op, not a red wall. It
  prints the demo credentials on success so the operator can copy them straight into the login form.
  - **ponytail:** seed creates users/tenants only — no ingested documents/chunks (that needs the
    ingestion pipeline plus provider keys, out of scope here). Ceiling: the Sources/Documents admin
    screens (later EPIC-11 stories) stay empty until real ingestion runs. Upgrade path: a richer seed
    (a demo source + a couple of ingested documents) once those screens exist and there's something
    to look at.

## Consequences
- An operator can go from a fresh stack to a working admin-UI login in three commands (`mise run
  up`, migrations, `mise run seed`), or wire `ragctl admin bootstrap` into a provisioning script for
  a real deployment's first admin.
- `internal/cp/auth.Service` gains one more method alongside `Signup`/`Login`, sharing their
  validation and hashing — no parallel password-handling path to keep in sync.
- The CLI grammar gains one nesting level (`ragctl admin bootstrap`), matching the `ragctl tenant
  <verb>` / `ragctl eval <verb>` grouping style already in `internal/cli`.
- Secrets discipline (SPEC-09 §2/§3) is preserved: the password never appears in a flag, and neither
  the plaintext nor the argon2id hash is ever printed by the command or the seed task's demo-creds
  banner (which prints the *known dev default*, not anything read back from the database).
