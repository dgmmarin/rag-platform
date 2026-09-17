#!/usr/bin/env bash
#MISE description="Smoke-check the observability stack (Prometheus, Loki, Grafana)"
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "smoke: docker not found — skipping"
  exit 0
fi

# Host ports follow the same overridable vars the compose file uses, so the
# check hits the same ports the stack bound (see .env.example).
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi
PROMETHEUS_PORT=${PROMETHEUS_PORT:-9090}
LOKI_PORT=${LOKI_PORT:-3100}
GRAFANA_PORT=${GRAFANA_PORT:-3000}

fail=0

check() {
  local name=$1 url=$2
  if curl -sf -m 5 "$url" >/dev/null 2>&1; then
    echo "PASS: $name ($url)"
  else
    echo "FAIL: $name ($url)"
    fail=1
  fi
}

check "prometheus ready" "http://localhost:${PROMETHEUS_PORT}/-/ready"
check "loki ready" "http://localhost:${LOKI_PORT}/ready"
check "grafana health" "http://localhost:${GRAFANA_PORT}/api/health"

targets_json=$(curl -sf -m 5 "http://localhost:${PROMETHEUS_PORT}/api/v1/targets" 2>/dev/null || echo "")
total=$(grep -o '"health":"[a-z]*"' <<<"$targets_json" | wc -l)
up=$(grep -o '"health":"up"' <<<"$targets_json" | wc -l)
if [[ $total -ge 2 && $up -eq $total ]]; then
  echo "PASS: prometheus targets ($up/$total up)"
else
  echo "FAIL: prometheus targets ($up/$total up)"
  fail=1
fi

exit "$fail"
