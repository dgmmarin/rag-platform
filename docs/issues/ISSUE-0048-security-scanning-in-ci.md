# ISSUE-0048: Security scanning in CI and dependency policy

**Type:** Feature · **Status:** Done · **Story:** STORY-10.6 · **Traces:** SPEC-09 §6, ADR-0014 (amended)

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
Turns the three SPEC-09 §6 security scans into CI gates that block merges on high severity:
Go modules (govulncheck), Python sidecar deps (pip-audit), and the container image (Trivy).
Replaces ADR-0014's blanket non-blocking govulncheck stance with a reachability + fixability
gate, and documents the dependency policy.

## Scope
- `mise-tasks/vulncheck-gate`: govulncheck `-format json` parsed with jq; FAILS on a called
  vulnerability (trace[0] has a function) in a non-stdlib module not in the allowlist; stdlib
  + allowlisted findings surfaced, non-blocking. Self-skips where tool/jq/network absent.
- `.ci/vuln-allowlist.txt`: the Go-1.22 pin-locked OSV ids (x/net, x/text, otel/sdk, grpc,
  pgx, aws-sdk, go-jose) — surfaced, non-blocking, deleted as the pin advances.
- `mise-tasks/pip-audit`: audits `services/parser/requirements.txt`, blocks on findings.
- `services/parser/requirements.txt`: **Flask 3.0.3 → 3.1.3** (clears PYSEC-2026-2151, an
  actionable finding — fixed, not allowlisted).
- `.github/workflows/ci.yml`: `vuln` job runs the gate (was `continue-on-error`); new
  `pip-audit` job; `image` job gains a Trivy scan (`HIGH,CRITICAL`, `ignore-unfixed`).
- `docs/dependency-policy.md`: scanners, blocking thresholds, the fixable-only principle.
- `docs/adr/0014-*` amended; this issue; backlog.

## Decisions
- Image scanner = **Trivy**; govulncheck gate = **fail only on non-stdlib called vulns**
  (user decisions). The gate red-walled on the CURRENT deps (213 called findings across the
  exact modules ADR-0014 named as pin-locked), so — settled by ADR-0014's already-accepted
  pin-locked exception — those OSV ids are allowlisted (surfaced, non-blocking) while any new
  non-stdlib called vuln blocks. No new ADR (ADR-0014 amended).

## Tests / runnable checks
- `mise run vulncheck-gate`: live scan → **PASS** (all called findings surfaced, none outside
  the allowlist). `bash -n` clean; self-skips without tool/jq/network.
- `mise run pip-audit`: found PYSEC-2026-2151 (Flask 3.0.3) → after the bump, **PASS** ("No
  known vulnerabilities"). `mise run test-parser` still green (13 passed) with Flask 3.1.3.
- Trivy runs in CI only (not installed locally); the `image` job builds then scans the same
  image (not rebuilt).
- `go build ./...` / `go vet ./...` / `gofmt` unaffected (no Go changed).

## Not in scope
- Advancing the Go 1.22 pin / bumping the pin-locked Go deps (a dedicated story; the
  allowlist tracks them).
- SBOM generation, license scanning.
