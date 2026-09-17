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

| Service    | URL                            |
|------------|--------------------------------|
| Grafana    | http://localhost:3000          |
| Prometheus | http://localhost:9090          |
| Loki       | http://localhost:3100          |

Grafana logs in as admin with no password.

## Changing the serve scrape target

Prometheus scrapes the `serve` process at `host.docker.internal:8091`. This
address is set in `deploy/obs/prometheus.yml`, under the `ragctl-serve` job.

If you change the `APP_PORT` value in `.env`, `serve` starts listening on a
different port. Update the port in `deploy/obs/prometheus.yml` to match, then
run `mise run obs-down` and `mise run obs` to reload Prometheus.

## Stop the stack

Run `mise run obs-down`. This stops the four containers. It does not stop
`serve` or `worker`; use `mise run services-down` for those.

## Smoke check

Run `bash deploy/obs/smoke.sh` after `mise run obs` to check that all four
services answer their health endpoints. The script skips itself with a
message if Docker is not installed.
