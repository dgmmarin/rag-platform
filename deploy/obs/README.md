# Observability stack

This stack shows metrics and logs from the local RAG platform. It has four
services: Prometheus, Loki, Promtail, and Grafana. Docker Compose starts them
behind the `obs` profile, so they do not start with the normal `docker compose
up` command.

- Prometheus collects metrics from the `serve` and `worker` processes.
- Loki stores logs.
- Promtail reads the log files in `.run/` and sends them to Loki.
- Grafana shows the metrics and logs on dashboards.

## Start order

Run these commands in order:

1. `mise run up` — starts Postgres and MinIO.
2. `mise run services` — starts `serve` and `worker` in the background. These
   write their logs to `.run/serve.log` and `.run/worker.log`.
3. `mise run obs` — starts Prometheus, Loki, Promtail, and Grafana.

Do not replace step 2 with `mise run api`. `mise run api` runs `serve` in the
foreground and prints logs to the terminal only. Promtail cannot read those
logs, so Loki and Grafana stay empty. Use `mise run services` (or
`serve-bg` and `worker-bg`) so the logs go to `.run/*.log`.

## URLs

| Service    | URL (default port)             | Override in `.env` |
|------------|--------------------------------|--------------------|
| Grafana    | http://localhost:3000          | `GRAFANA_PORT`     |
| Prometheus | http://localhost:9090          | `PROMETHEUS_PORT`  |
| Loki       | http://localhost:3100          | `LOKI_PORT`        |

Grafana logs in as admin with no password.

Each host port is overridable so it does not collide with another local stack.
If a start fails with "address already in use", set the matching `*_PORT` in
`.env` to a free port (for example `GRAFANA_PORT=13000`) and run `mise run obs`
again. `mise run obs` and the smoke check read these vars, so their printed URLs
follow the ports you set.

## Changing the serve scrape target

Prometheus scrapes the `serve` process at `host.docker.internal:8091`. This
address is set in `deploy/obs/prometheus.yml`, under the `ragctl-serve` job.

serve's listen address is set by `RAGCTL_ADDR` in `.env`, not `APP_PORT`
(`APP_PORT` only applies to the `app` compose profile). If you change
`RAGCTL_ADDR`, update the `ragctl-serve` target in `deploy/obs/prometheus.yml`
to match, then run `mise run obs-down` and `mise run obs` to reload Prometheus.

## Stop the stack

Run `mise run obs-down`. This stops the four containers. It does not stop
`serve` or `worker`; use `mise run services-down` for those.

## Smoke check

Run `bash deploy/obs/smoke.sh` after `mise run obs` to check that all four
services answer their health endpoints. The script skips itself with a
message if Docker is not installed.
