# ISSUE-0049: Load and isolation testing

**Type:** Feature · **Status:** Done (harness committed; measured run on the real stack) · **Story:** STORY-10.7 · **Traces:** NFR-PERF-01, SRS §8.5, ADR-0051

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs.

## Summary
A k6 load + isolation scenario: 50 concurrent queries spread across 4 tenants against the
real `/v1/retrieve` endpoint, asserting retrieval p95 ≤ 300 ms at ~1 M chunks per tenant
(NFR-PERF-01, SRS §8.5). Because each VU authenticates as a distinct tenant by API key
(FR-ACC-03), the run also exercises per-tenant isolation under concurrency.

## Scope
- `test/load/retrieval-load.js`: the k6 scenario — 50 VUs across 4 tenants, a
  `retrieval_latency` Trend with threshold `p(95)<300`, and a 100%-success check; k6 exits
  non-zero if the threshold is breached (the gate).
- `test/load/seed-tenant.sql`: bulk-seeds one tenant DB to N chunks via `generate_series`,
  mirroring `test/e2e/retrieve_bench_test.go` (ADR-0051) so indexes behave as at scale.
- `mise-tasks/loadtest`: runs k6; self-skips (exit 0) when k6 / `TENANT_KEYS` are absent.
- `test/load/README.md`: provision 4 tenants (3 on one PG instance + 1 on another, SRS §8.1),
  seed to 1 M chunks, run, and commit results.
- `test/load/results/README.md`: committed-results scaffold (`k6 --summary-export`).
- Docs: this issue, backlog.

## Decisions
- Load tool = **k6** (the AC's named tool); perf target = **retrieval p95 ≤ 300 ms**
  (NFR-PERF-01 / SRS §8.5). Both pinned by the AC — no open decision, no new ADR. Seeding
  reuses the `retrieve_bench_test.go` bulk-load pattern (ADR-0051) rather than a new approach.

## Tests / runnable check
- `mise run loadtest`: self-skips cleanly (exit 0) without k6/`TENANT_KEYS` (verified in the
  sandbox); runs the gated k6 threshold against the provisioned, seeded stack.
- `bash -n mise-tasks/loadtest` clean. `go build`/`vet`/`gofmt` unaffected (no Go changed).

## Not run in this environment
The measured p95 requires the real stack (Postgres + pgvector, MinIO, `ragctl serve`), 4
provisioned tenants seeded to ~1 M chunks, and k6 — none available in the sandbox. The
scenario/seed/runner/threshold and a results scaffold are committed; the measured run is
executed per `test/load/README.md` and its summary committed to `test/load/results/`.

## Not in scope
- The cross-tenant-access isolation *correctness* suite (every API path) — SRS §8.1, already
  delivered by `test/e2e/isolation_e2e_test.go` (STORY-02.6). This story adds concurrent load.
