// Retrieval load + isolation test (STORY-10.7, NFR-PERF-01, SRS §8.5).
//
// 50 concurrent queries spread across 4 tenants against the REAL /v1/retrieve
// endpoint (retrieval-only — no LLM), asserting retrieval p95 <= 300 ms. Each VU
// authenticates as a distinct tenant via its API key (FR-ACC-03: the tenant comes
// from the credential, never a request parameter), so the run also exercises
// per-tenant isolation under concurrency.
//
// Prerequrisites (see README.md): the stack up, 4 tenants each seeded to ~1M chunks
// (seed-tenant.sql), and their query-scope API keys passed as env:
//   BASE_URL   e.g. http://localhost:8080
//   TENANT_KEYS  comma-separated: "key1,key2,key3,key4"
//
// Run: `mise run loadtest` (or `k6 run test/load/retrieval-load.js`). k6 exits
// non-zero if the p95 threshold is breached or any request fails — that IS the gate.
import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/$/, '');
const KEYS = (__ENV.TENANT_KEYS || '').split(',').map((s) => s.trim()).filter(Boolean);
const VUS = parseInt(__ENV.VUS || '50', 10);
const DURATION = __ENV.DURATION || '1m';

// A few representative queries; the seed corpus carries "topic N" / "error code EN"
// full-text terms (see seed-tenant.sql), so these match real rows at scale.
const QUERIES = [
  'widget maintenance guide',
  'error code E42',
  'topic 137 overview',
  'how to reset the device',
  'troubleshooting steps',
];

// Retrieval latency is tracked in its own Trend so the threshold is on retrieval,
// not on any incidental request.
const retrievalLatency = new Trend('retrieval_latency', true);

export const options = {
  vus: VUS,
  duration: DURATION,
  thresholds: {
    // NFR-PERF-01 / SRS §8.5: retrieval-only p95 <= 300 ms.
    retrieval_latency: ['p(95)<300'],
    // Every retrieval must succeed (isolation/auth failures would show here).
    checks: ['rate==1.0'],
  },
};

export function setup() {
  if (KEYS.length < 4) {
    throw new Error(
      `TENANT_KEYS must list 4 tenant API keys (got ${KEYS.length}). ` +
        `See test/load/README.md for provisioning + seeding.`,
    );
  }
  return { keys: KEYS };
}

export default function (data) {
  // Spread VUs evenly across the 4 tenants (deterministic by VU id).
  const key = data.keys[(exec.vu.idInTest - 1) % data.keys.length];
  const q = QUERIES[exec.scenario.iterationInInstance % QUERIES.length];

  const res = http.post(
    `${BASE_URL}/v1/retrieve`,
    JSON.stringify({ query: q, top_k: 10 }),
    { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` }, tags: { endpoint: 'retrieve' } },
  );

  retrievalLatency.add(res.timings.duration);
  check(res, {
    'status is 200': (r) => r.status === 200,
    'body has results field': (r) => {
      try {
        return Object.prototype.hasOwnProperty.call(r.json(), 'results');
      } catch (_e) {
        return false;
      }
    },
  });
}
