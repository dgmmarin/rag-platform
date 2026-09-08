# ADR-0014: CI drives mise tasks; coverage gate lives in a shell lib and skips not-yet-created packages

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** NFR-MNT-03, STORY-01.3

## Context
STORY-01.3 adds the CI pipeline: on a PR, run lint, unit tests with a coverage
report, integration tests against Postgres, `govulncheck`, and an image build,
with a 70% coverage gate on `internal/tenant`, `internal/ingest`,
`internal/retrieve`, `internal/connector`. Two forces shape the design:

1. The mandatory-practices policy says every build/test/lint/run command goes
   through a `mise` task, and CI must run the SAME commands a developer runs so
   local and CI never diverge.
2. Three of the four gated packages do not exist yet (they land in EPIC-02+).
   A naive gate would read their coverage as 0% and fail CI forever, or force
   creating empty stub packages just to satisfy the gate.

There is also a standing constraint: `go.mod` is pinned to `go 1.22` with no
`toolchain` directive (prior-story decision, to keep OTel/Prometheus/age/AWS-SDK
versions compatible). That pin interacts badly with `govulncheck`.

## Options
1. Encode lint/test/coverage/vuln steps directly in the workflow YAML.
   Rejected: duplicates logic, drifts from local, violates the mise policy.
2. Gate all four packages unconditionally. Rejected: red-walls CI until every
   package exists, or forces empty stub packages (dead code with no trace).
3. CI invokes `mise run <task>`; the coverage-gate logic lives in a sourced
   shell library (`mise-tasks/lib/coverage.sh`), config-driven via
   `.ci/coverage-packages.txt`, and enforces a gated package only once it
   exists (SKIP otherwise). `govulncheck` runs on every PR but is non-blocking
   while the Go 1.22 pin makes its stdlib findings unfixable.

## Decision
Option 3.

- **CI drives mise.** Each job runs `mise run lint` / `mise run coverage` /
  `mise run e2e` / `mise run vuln`, and `docker build -f Dockerfile .` for the
  image. `jdx/mise-action` installs mise so the task scripts are identical to
  local. `actions/setup-go` pins `go-version: '1.22'` to match `go.mod`.
- **Gate logic in a shell lib, not YAML.** `mise-tasks/lib/coverage.sh` defines
  `eval_coverage_gate <threshold> <module> <gated> <existing> <func>`. It parses
  `go tool cover -func`, aggregates per package by prefix, and for each gated
  package: SKIP if it does not exist yet, OK if it meets the threshold, BELOW
  (and non-zero exit) if it exists and is under. `mise-tasks/coverage` runs the
  unit suite with a profile, emits `coverage/coverage-func.txt` +
  `coverage.html` artifacts, then applies the gate. Local and CI share it byte
  for byte. The logic is TDD-covered by `test/coverage/coverage_gate_test.sh`.
- **Config-driven gated list.** `.ci/coverage-packages.txt` holds the four
  packages, one per line (comments allowed). Authored ahead of the code; the
  SKIP behaviour means it auto-enforces the moment each package lands.
- **Integration == the real compose stack.** The AC's "Postgres service" is the
  `//go:build e2e` suite, which needs Postgres+pgvector, MinIO, and the parser
  sidecar. CI runs `mise run e2e`, which depends on `up` (`docker compose up -d
  --wait`) — the identical path to local — rather than hand-crafted GitHub
  service containers, so CI and local stand up the same services.
- **govulncheck runs but is non-blocking (for now).** `mise run vuln` runs
  `govulncheck ./...` (pinned `v1.1.4`, Go 1.22-compatible). Under the Go 1.22
  pin, govulncheck reports Go standard-library advisories fixed only in Go
  1.23/1.25, which cannot be remediated without moving the pin. Making the step
  a hard gate would red-wall CI indefinitely on unactionable findings, so the
  `vuln` job is `continue-on-error: true`: the scan still runs on every PR and
  its findings are visible in the logs. Flip it to blocking once the Go pin
  advances past these advisories (or the dependency-level findings — pgx,
  x/net, x/text, grpc, otel/sdk — are bumped under a dedicated story).

## Consequences
- No coverage/gate/vuln logic is duplicated in YAML; changing the gate is a
  one-file shell edit exercised by an existing test.
- CI is green today (all four gated packages SKIP) and starts enforcing 70%
  automatically as EPIC-02+ packages land — no workflow edit required.
- The non-blocking `vuln` job is an explicit, time-boxed exception tied to the
  Go 1.22 pin, not a silent suppression; the findings remain surfaced. This
  must be revisited when the Go pin moves.
- `mise.toml` stays minimal (tool pins only); new behaviour is task scripts and
  a shell lib under `mise-tasks/`.

## Amendment (STORY-10.6, 2026-09-08) — security scanning becomes a gate

SPEC-09 §6 requires dependency scanning (govulncheck, pip-audit) and image scanning
in CI, and STORY-10.6's AC makes them block merges on high severity. This supersedes
the "govulncheck runs but is non-blocking" decision above.

- **govulncheck is now a gate** (`mise run vulncheck-gate`, replacing the
  `continue-on-error` job). It parses `govulncheck -format json` and FAILS on a
  *called* vulnerability (a finding whose `trace[0]` has a function) in a *non-stdlib*
  module that is not allowlisted. Go standard-library advisories — unfixable under the
  `go 1.22` pin — are printed but never fail (the original rationale, now enforced by
  code, not a blanket `continue-on-error`).
- **A small allowlist** (`.ci/vuln-allowlist.txt`) carries the OSV ids of the
  pin-locked dependency set this ADR already named (pgx, x/net, x/text, grpc, otel/sdk,
  plus aws-sdk and go-jose found by the live scan) — remediated only by moving the Go
  pin (a dedicated story). They are surfaced, non-blocking. A NEW non-stdlib called vuln
  outside the list blocks merge. Emptying the list as the pin advances tightens the gate
  automatically. This is the code-enforced form of this ADR's stdlib exception, extended
  to the same pin-locked class — not a silent suppression (every id is printed each run,
  with a note, and the file records why).
- **pip-audit** (`mise run pip-audit`) audits `services/parser/requirements.txt` and
  blocks on any finding. (STORY-10.6 bumped Flask 3.0.3 → 3.1.3 to clear PYSEC-2026-2151,
  an actionable finding with no pin constraint — fixed rather than allowlisted.)
- **Image scanning = Trivy** (`aquasecurity/trivy-action`) on the image the `image` job
  already builds, failing on `HIGH,CRITICAL` with `ignore-unfixed: true` (an unfixed
  base-image CVE cannot gate a merge — the same fixable-only principle as govulncheck).
- Policy is documented in `docs/dependency-policy.md`; the tasks stay mise-driven and
  CI keeps invoking `mise run <task>` (this ADR's core decision is unchanged).
