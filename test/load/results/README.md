# Load-test results

Committed results of the retrieval load + isolation test (`../retrieval-load.js`,
STORY-10.7, NFR-PERF-01 / SRS §8.5). Each run drops a `k6 run --summary-export`
JSON here (e.g. `latest.json`) plus a one-line entry in the table below.

Target: **retrieval p95 ≤ 300 ms** at 50 concurrent queries across 4 tenants,
~1 M chunks each.

| Date | Commit | Tenants × chunks | VUs / duration | Retrieval p95 | Result | Summary file |
|---|---|---|---|---|---|---|
| _pending_ | — | 4 × 1,000,000 | 50 / 1m | _—_ | _not yet run_ | — |

> Not yet run in this environment: the scenario, seed script, and runner are committed,
> but the measured run requires the real stack (Postgres + pgvector, MinIO, `ragctl serve`),
> 4 provisioned tenants seeded to ~1 M chunks, and k6 — none available in the sandbox.
> Run per `../README.md` and replace the pending row with the exported summary. k6's
> `retrieval_latency p(95)<300` threshold makes the run itself pass/fail.
