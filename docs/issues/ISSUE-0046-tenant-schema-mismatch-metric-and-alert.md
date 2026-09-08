# ISSUE-0046: Tenant migration mismatch metric and alert

**Type:** Feature · **Status:** Todo · **Story:** STORY-10.2 follow-up · **Traces:** SPEC-10 §5, SPEC-10 §2, ISSUE-0045

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
SPEC-10 §5 lists an alert "tenant migration mismatch count > 0", but the §2 metric catalogue
defines no migration/schema-mismatch metric, so STORY-10.2 (ISSUE-0045) could not commit that
alert without pointing it at a non-existent series. This issue tracks closing the §5↔§2 gap:
add the metric, then add the alert.

## Background
The resolver already detects drift per request — `Open` returns `ErrSchemaOutdated` when a
tenant's `schema_version` != the binary's expected tenant migration version (SPEC-01 §7) — but
it keeps no fleet-wide count, so there is nothing to alert on. The other five §5 alerts shipped
in STORY-10.2 (`deploy/prometheus/rules/ragctl.rules.yml`).

## Scope (proposed)
- Add a `tenant_schema_mismatch` gauge to the SPEC-10 §2 catalogue (append-only): the number of
  active tenants whose `schema_version` is behind the expected version. Label minimal (no
  per-tenant label needed for a fleet count; or `severity`/none per the cardinality guard).
- Emit it from a periodic fleet scan (mirroring the SPEC-10 §4 readiness cache refresh): read
  each active tenant's `schema_version` from the control-plane registry and compare to the
  expected version. This is control-plane registry data only (C-3) — no tenant DB read needed.
  Refresh on an interval (e.g. 60 s), like the readiness embedding ping.
- Add the alert to `deploy/prometheus/rules/ragctl.rules.yml`:
  `tenant_schema_mismatch > 0` (SPEC-10 §5), with a runbook link, and extend
  `internal/obs/alertrules_test.go`'s known-metric set with the new metric.
- Update SPEC-10 §2 (new metric row) + §5 (mark the alert delivered), and this issue.

## Decisions to make when picked up
- Where the fleet scan runs (API server vs worker vs both) and its interval.
- Whether the gauge is unlabeled (fleet count) or labelled (e.g. per expected-vs-actual version)
  — respect the §2 cardinality guard.
- Whether a new ADR is warranted (a periodic fleet-scan collector is arguably a small decision).

## Not in scope
- Anything beyond the migration-mismatch metric + its single alert.
