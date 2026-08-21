# shellcheck shell=bash
# Coverage-gate logic for STORY-01.3 (traces NFR-MNT-03).
#
# Config-driven per-package coverage gate. It enforces a minimum coverage ONLY
# for packages that (a) appear in the gated config AND (b) already exist in the
# module. Gated packages that do not exist yet (e.g. internal/tenant before
# EPIC-02) are reported as SKIP and never fail the gate, so the gate can be
# wired now and starts enforcing automatically once those packages land.
#
# This file is a library: it is sourced (by the RED test and by mise-tasks/
# coverage). It defines functions only and runs nothing at source time.

# _cov_pkg_avg <module> <pkg-relpath> <go-cover-func-file>
# Prints the average coverage percentage (as a plain number, e.g. 85.0) across
# all per-func lines whose file belongs to <module>/<pkg-relpath>. Prints
# nothing if no lines match. Uses `go tool cover -func` output format:
#   <module>/<pkg>/<file>.go:<line>:\t<func>\t<pct>%
_cov_pkg_avg() {
  local mod="$1" pkg="$2" funcfile="$3"
  local prefix="${mod}/${pkg}/"
  awk -v prefix="$prefix" '
    index($0, prefix) == 1 {
      # last field looks like "80.0%"; strip the trailing percent sign.
      pct = $NF
      sub(/%$/, "", pct)
      sum += pct
      n++
    }
    END {
      if (n > 0) printf "%.1f", sum / n
    }
  ' "$funcfile"
}

# eval_coverage_gate <threshold> <module> <gated-pkgs-file> <existing-pkgs-file> <go-cover-func-file>
#
#   threshold          integer/float minimum percent (e.g. 70)
#   module             go module path (e.g. github.com/rag-platform/ragctl)
#   gated-pkgs-file    one gated package per line, module-relative
#                      (e.g. internal/tenant). '#' comments and blanks ignored.
#   existing-pkgs-file one existing package per line, module-qualified
#                      (e.g. <module>/internal/tenant) — typically `go list ./...`.
#   go-cover-func-file output of `go tool cover -func=<profile>`.
#
# For each gated package:
#   - not in existing list        -> "<pkg> SKIP (package not present)"       (no fail)
#   - exists, coverage >= thresh   -> "<pkg> OK <pct>%"                        (no fail)
#   - exists, coverage <  thresh   -> "<pkg> BELOW <pct>% < <thresh>%"         (FAIL)
#
# Returns non-zero iff at least one existing gated package is below threshold.
eval_coverage_gate() {
  local threshold="$1" mod="$2" gated_file="$3" existing_file="$4" func_file="$5"
  local violations=0
  local pkg qualified avg

  # Slurp inputs once. The callers may pass process substitutions (/dev/fd/N),
  # which are single-read streams; re-reading them per package would drain the
  # FD after the first lookup. Read gated packages and existing packages up
  # front, and materialise the func output in a temp file so _cov_pkg_avg can
  # scan it once per package.
  local existing_list gated_list func_tmp
  existing_list="$(cat "$existing_file")"
  gated_list="$(cat "$gated_file")"
  func_tmp="$(mktemp)"
  cat "$func_file" >"$func_tmp"

  while IFS= read -r pkg || [[ -n "$pkg" ]]; do
    # strip comments / whitespace / blank lines
    pkg="${pkg%%#*}"
    pkg="${pkg#"${pkg%%[![:space:]]*}"}"
    pkg="${pkg%"${pkg##*[![:space:]]}"}"
    [[ -z "$pkg" ]] && continue

    qualified="${mod}/${pkg}"
    if ! grep -qxF "$qualified" <<<"$existing_list"; then
      echo "${pkg} SKIP (package not present)"
      continue
    fi

    avg="$(_cov_pkg_avg "$mod" "$pkg" "$func_tmp")"
    if [[ -z "$avg" ]]; then
      # Package exists but produced no coverage lines: treat as 0%.
      avg="0.0"
    fi

    if awk -v a="$avg" -v t="$threshold" 'BEGIN { exit !(a + 0 < t + 0) }'; then
      echo "${pkg} BELOW ${avg}% < ${threshold}%"
      violations=$((violations + 1))
    else
      echo "${pkg} OK ${avg}%"
    fi
  done <<<"$gated_list"

  rm -f "$func_tmp"
  [[ "$violations" -eq 0 ]]
}
