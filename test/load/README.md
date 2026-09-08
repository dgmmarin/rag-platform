# Retrieval load + isolation test

STORY-10.7, NFR-PERF-01, SRS §8.5. Drives **50 concurrent queries across 4 tenants**
against the real `/v1/retrieve` endpoint and asserts **retrieval p95 ≤ 300 ms** at
~1 M chunks per tenant. Because each VU authenticates as a distinct tenant by API key
(FR-ACC-03), it also exercises per-tenant isolation under concurrency.

Load tool: **k6** (the AC's named tool). k6's threshold engine is the pass/fail gate —
it exits non-zero if p95 > 300 ms or any request fails.

## Files

| File | Purpose |
|---|---|
| `retrieval-load.js` | The k6 scenario (50 VUs, 4 tenants, `retrieval_latency p(95)<300`). |
| `seed-tenant.sql` | Bulk-seeds one tenant DB to N chunks (mirrors `retrieve_bench_test.go`, ADR-0051). |
| `results/` | Committed run results (`k6 run --summary-export`). |

## Run it (against the real stack)

k6 and a running stack are not available in the sandbox; run this where they are.

1. **Provision 4 tenants** (SRS §8.1: three on one Postgres instance, a fourth on a
   separate instance) and mint a **query-scope API key** for each:
   ```
   ragctl enroll --slug t1 --name "Tenant 1"   # ... t2, t3 on instance A; t4 on instance B
   # create a query-scoped API key per tenant (admin API / ragctl), collect the 4 secrets
   ```
2. **Seed each tenant to ~1 M chunks** (dim must equal the tenant's embedding dimension):
   ```
   psql "$T1_DB_URL" -v source_id="'11111111-1111-1111-1111-111111111111'" \
     -v n_docs=100000 -v chunks_per_doc=10 -v dim=1536 -f test/load/seed-tenant.sql
   # repeat for t2..t4
   ```
   (100000 × 10 = 1,000,000 chunks. HNSW index maintenance on insert dominates load time.)
3. **Run k6** via the mise task:
   ```
   BASE_URL=http://localhost:8080 \
   TENANT_KEYS="$T1_KEY,$T2_KEY,$T3_KEY,$T4_KEY" \
   mise run loadtest
   ```
   Optional: `VUS` (default 50), `DURATION` (default 1m).
4. **Commit the results:** export and drop the summary under `results/`:
   ```
   BASE_URL=... TENANT_KEYS=... k6 run --summary-export test/load/results/latest.json test/load/retrieval-load.js
   ```
   Record the p95 and pass/fail in `results/README.md`.

## Runnable check

`mise run loadtest` self-skips (exit 0) when k6 or `TENANT_KEYS` is absent (the sandbox
and per-PR CI), so it is safe to invoke anywhere; the real gated run happens against the
provisioned, seeded stack. The existing Go micro-benchmark `test/e2e/retrieve_bench_test.go`
(`mise run e2e` scale knobs) covers single-tenant retrieval latency at scale; this k6
scenario adds the multi-tenant concurrent HTTP load the AC specifies.
