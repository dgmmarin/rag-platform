#!/usr/bin/env bash
#MISE description="Smoke-check the observability stack (Prometheus, Loki, Grafana)"
set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "smoke: docker not found — skipping"
  exit 0
fi

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

check "prometheus ready" "http://localhost:9090/-/ready"
check "loki ready" "http://localhost:3100/ready"
check "grafana health" "http://localhost:3000/api/health"

targets_json=$(curl -sf -m 5 "http://localhost:9090/api/v1/targets" 2>/dev/null || echo "")
total=$(grep -o '"health":"[a-z]*"' <<<"$targets_json" | wc -l)
up=$(grep -o '"health":"up"' <<<"$targets_json" | wc -l)
if [[ $total -ge 2 && $up -eq $total ]]; then
  echo "PASS: prometheus targets ($up/$total up)"
else
  echo "FAIL: prometheus targets ($up/$total up)"
  fail=1
fi

exit "$fail"
