# Dependency and vulnerability-scanning policy

**Traces:** SPEC-09 §6, NFR-MNT-03. **Story:** STORY-10.6. **Decision:** ADR-0014 (amended).

Every pull request runs three security scans as CI gates. Each is a `mise run <task>`
so local and CI are identical (ADR-0014).

## Scanners and what blocks a merge

| Layer | Scanner | Task / CI job | Blocks merge on |
|---|---|---|---|
| Go modules + first-party code | govulncheck | `mise run vulncheck-gate` (`vuln` job) | a **called** vulnerability in a **non-stdlib** module **not** allowlisted |
| Python (parser sidecar) | pip-audit | `mise run pip-audit` (`pip-audit` job) | any known vulnerability in `services/parser/requirements.txt` |
| Container image | Trivy | `image` job (`aquasecurity/trivy-action`) | a **fixable** `HIGH` or `CRITICAL` image/OS-package vulnerability |

"High severity" is realised per scanner: for govulncheck, a *called* (reachable) vuln
is the actionable signal (it has no CVSS field); for Trivy, `severity: HIGH,CRITICAL`.

## The fixable-only principle

A gate must block on vulnerabilities the team **can act on**, and must not permanently
red-wall CI on findings that are currently unfixable — otherwise the signal is ignored.
So:

- **govulncheck** surfaces but does not block Go **standard-library** advisories: they
  are fixed only in Go ≥ 1.23/1.25, and `go.mod` is pinned to `go 1.22` (no `toolchain`)
  to keep OTel / AWS-SDK / pgx / grpc / x/net / x/text mutually compatible (ADR-0014).
- The same applies to the **pin-locked dependency set** those advisories live in:
  their OSV ids are listed in [`.ci/vuln-allowlist.txt`](../.ci/vuln-allowlist.txt),
  surfaced on every run but non-blocking, remediated only by advancing the Go pin (a
  dedicated dependency-bump story). A **new** non-stdlib called vuln outside that list
  **blocks** merge.
- **Trivy** uses `ignore-unfixed: true`: an image/OS-package CVE with no released fix
  cannot gate a merge; it blocks once a fix exists.
- **pip-audit** has **no** allowlist: Python deps carry no equivalent pin constraint, so
  findings are fixed by bumping. (STORY-10.6 bumped Flask 3.0.3 → 3.1.3 to clear
  PYSEC-2026-2151.)

## Handling a finding

1. **Fix it** — bump the dependency to a fixed version (preferred; e.g. the Flask bump).
2. If a Go dependency's fix requires Go > 1.22, it belongs to the pin-locked set: add its
   OSV id to `.ci/vuln-allowlist.txt` with a one-line reason, and it is tracked for the
   Go-pin-advance story. Do **not** allowlist a finding that is fixable under the pin.
3. When the Go pin advances, bump the pin-locked deps and delete their allowlist lines —
   the gate then enforces them automatically.

## Where the logic lives

Scanner invocation and thresholds are in `mise-tasks/` (`vulncheck-gate`, `pip-audit`)
and `.github/workflows/ci.yml` (the `image` job's Trivy step); the allowlist is
`.ci/vuln-allowlist.txt`. No scan logic is duplicated in YAML (ADR-0014).
