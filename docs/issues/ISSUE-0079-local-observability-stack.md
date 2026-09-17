# ISSUE-0079: Local observability stack (Prometheus, Loki, Promtail, Grafana)

**Type:** Chore/Feature · **Status:** Done · **Priority:** Medium · **Traces:** SPEC-10 §2/§5, ADR-0078, ADR-0011

## Summary
`serve` and the worker already exposed Prometheus metrics and wrote structured JSON logs, but nothing
local scraped, stored, or showed them, so answering "what happened to this source's syncs?" needed a
manual SQL query. This adds an opt-in `obs` compose profile, four services, that scrapes the existing
metrics endpoints and ships the existing log files into a queryable dashboard.

## What was built
- **Four services behind the `obs` compose profile** (`docker-compose.yml`, `profiles: ["obs"]`,
  mirrors the ADR-0011 `app` profile so a plain `mise run up` is unaffected):
  - **Prometheus** scrapes `serve`'s `/metrics` and the worker's `:9091/metrics` at
    `host.docker.internal`, since both run on the host. Config: `deploy/obs/prometheus.yml`.
  - **Loki** stores logs on the filesystem. Config: `deploy/obs/loki.yml`.
  - **Promtail** tails the host log files `.run/serve.log` and `.run/worker.log`, parses each line as
    JSON, promotes `service`, `event`, `kind`, `status`, `tenant_id`, `source_id` to labels, and ships
    them to Loki. Config: `deploy/obs/promtail.yml`; `.run/` is bind-mounted read-only.
  - **Grafana** is provisioned with Prometheus and Loki datasources and one Jobs dashboard
    (`deploy/obs/grafana/`): queue depth, job throughput by kind/status, job duration, failure rate,
    embedding-reuse rate, and a Logs panel filtered to job events. Anonymous admin, no login, local
    only.
- **`mise run obs`** runs `docker compose --profile obs up -d --wait` and prints the Grafana and
  Prometheus URLs (reading `GRAFANA_PORT`/`PROMETHEUS_PORT` from `.env` if set). **`mise run obs-down`**
  runs `docker compose --profile obs down`.
- **Overridable host ports.** `GRAFANA_PORT` (default 3000), `PROMETHEUS_PORT` (default 9090), and
  `LOKI_PORT` (default 3100) in `.env` control the bound host ports, so the stack does not collide
  with another local Grafana or dev server already using 3000.
- **Documented start order** (`deploy/obs/README.md`): `mise run up` (Postgres, MinIO), then
  `mise run services` (starts `serve` and worker in the background, writing `.run/*.log`), then
  `mise run obs`. Running `mise run api` instead of `mise run services` prints logs to the terminal
  only, not to `.run/*.log`, so Promtail has nothing to tail and Loki/Grafana stay empty.
- **Smoke check** (`deploy/obs/smoke.sh`, not part of CI): after `mise run obs`, checks Prometheus
  `/-/ready` and both scrape targets `up`, Loki `/ready`, and Grafana `/api/health`. It checks
  component health and target-up counts, not a specific log line in Loki. Self-skips when Docker is
  unavailable.

## Tests
- `deploy/obs/smoke.sh` exercises the running stack end to end (documented as a manual/local check,
  not a CI gate, since it needs Docker and the host services running).

## Related
ADR-0078 (the decision record for this stack and its file-tail design), ADR-0011 (the compose profile
pattern this follows), ISSUE-0078 (the lifecycle log events this stack visualises), ADR-0067 (the
metrics this stack scrapes).
