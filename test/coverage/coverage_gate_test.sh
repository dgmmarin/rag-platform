#!/usr/bin/env bash
# TDD test for the coverage-gate logic (STORY-01.3, traces NFR-MNT-03).
#
# The gate is config-driven (see .ci/coverage-packages.txt): it enforces a
# minimum per-package coverage ONLY for packages that already exist and appear
# in the config. Packages listed but not yet created (e.g. internal/tenant
# before STORY-02.x) must be skipped, so wiring the gate now "just works" once
# those packages land. This asserts that behaviour against synthetic
# `go tool cover -func` output — no Go build required.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../../mise-tasks/lib/coverage.sh
source "$ROOT/mise-tasks/lib/coverage.sh"

fail=0
check() {
  local name="$1" want="$2" got="$3"
  if [[ "$want" == "$got" ]]; then
    echo "ok   - $name"
  else
    echo "FAIL - $name: want [$want] got [$got]"
    fail=1
  fi
}

MOD="github.com/rag-platform/ragctl"

# Synthetic `go tool cover -func` totals: "<pkg>/<file>:line: func  NN.N%".
# Only per-file lines are needed; the gate aggregates per package by prefix.
funcout() {
  cat <<EOF
${MOD}/internal/tenant/db.go:10:	Foo	80.0%
${MOD}/internal/tenant/db.go:20:	Bar	90.0%
${MOD}/internal/ingest/x.go:5:	Baz	40.0%
${MOD}/internal/cli/cli.go:5:	Ignored	10.0%
total:					(statements)	55.0%
EOF
}

# Config: gated packages + threshold. tenant exists (85% avg, PASS),
# ingest exists (40%, FAIL), retrieve+connector do NOT exist (SKIP).
existing_pkgs() {
  cat <<EOF
${MOD}/internal/tenant
${MOD}/internal/ingest
EOF
}
gated_pkgs() {
  cat <<EOF
internal/tenant
internal/ingest
internal/retrieve
internal/connector
EOF
}

# 1. A package above threshold passes.
out="$(eval_coverage_gate 70 "$MOD" <(gated_pkgs) <(existing_pkgs) <(funcout))"
rc=$?
check "tenant (85%) does not appear as a violation" "" "$(echo "$out" | grep 'internal/tenant.*BELOW' || true)"

# 2. A package below threshold is reported and gate fails (rc != 0).
check "ingest (40%) reported BELOW" "1" "$(echo "$out" | grep -c 'internal/ingest.*BELOW')"
check "gate returns non-zero on violation" "1" "$([[ $rc -ne 0 ]] && echo 1 || echo 0)"

# 3. Non-existent gated packages are skipped, not treated as 0% failures.
check "retrieve skipped (not yet created)" "1" "$(echo "$out" | grep -c 'internal/retrieve.*SKIP')"
check "connector skipped (not yet created)" "1" "$(echo "$out" | grep -c 'internal/connector.*SKIP')"

# 4. When every existing gated package passes, the gate returns zero.
existing_pass() { echo "${MOD}/internal/tenant"; }
gated_pass() { echo "internal/tenant"; }
eval_coverage_gate 70 "$MOD" <(gated_pass) <(existing_pass) <(funcout) >/dev/null
check "gate returns zero when all existing gated packages pass" "0" "$?"

# 5. When NO gated package exists yet, the gate is a no-op success.
none_exist() { :; }
eval_coverage_gate 70 "$MOD" <(gated_pkgs) <(none_exist) <(funcout) >/dev/null
check "gate is no-op success when no gated package exists yet" "0" "$?"

exit $fail
