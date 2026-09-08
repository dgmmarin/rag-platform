#!/usr/bin/env bash
# Monthly PITR restore drill (STORY-10.5, NFR-REL-03, ADR-0068).
#
# Restores a tenant Postgres cluster from the pgBackRest repo (MinIO/S3) to a
# THROWAWAY target at a point in time, starts it, and asserts it recovered — so we
# know the backups are restorable, not merely present. Run monthly (see
# docs/runbooks/backup-and-pitr.md); run it inside the Postgres container that has
# pgBackRest + the repo configured, e.g.:
#   docker compose exec postgres bash /path/to/restore-drill.sh
#
# Runnable check: this script is `bash -n` clean and SELF-SKIPS (exit 0) where
# pgBackRest / Postgres binaries or a configured repo are absent (e.g. a CI runner),
# so `mise run backup-drill` is safe to invoke anywhere; the full drill runs only
# where the tooling and repo exist.
set -euo pipefail

STANZA="${STANZA:-main}"
TARGET_TIME="${TARGET_TIME:-}"                 # empty = restore to latest; else 'YYYY-MM-DD HH:MM:SS+00'
RESTORE_PATH="${RESTORE_PATH:-$(mktemp -d)/pgdata-drill}"
DRILL_PORT="${DRILL_PORT:-6544}"

log() { printf '[restore-drill] %s\n' "$*"; }

# --- Preconditions: skip cleanly (success) when the drill cannot run here. ----
missing=""
for bin in pgbackrest pg_ctl psql pg_controldata; do
  command -v "$bin" >/dev/null 2>&1 || missing="$missing $bin"
done
if [ -n "$missing" ]; then
  log "SKIP: missing tools:$missing — the drill runs where pgBackRest + Postgres exist"
  exit 0
fi
if ! pgbackrest --stanza="$STANZA" info >/dev/null 2>&1; then
  log "SKIP: stanza '$STANZA' has no reachable repository here (nothing to restore)"
  exit 0
fi

cleanup() {
  pg_ctl -D "$RESTORE_PATH" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$RESTORE_PATH" 2>/dev/null || true
}
trap cleanup EXIT

log "restoring stanza='$STANZA' to '$RESTORE_PATH' (target=${TARGET_TIME:-latest})"
mkdir -p "$RESTORE_PATH"

if [ -n "$TARGET_TIME" ]; then
  pgbackrest --stanza="$STANZA" --pg1-path="$RESTORE_PATH" \
    --type=time --target="$TARGET_TIME" --target-action=promote restore
else
  pgbackrest --stanza="$STANZA" --pg1-path="$RESTORE_PATH" restore
fi

# Sanity #1: the restored cluster's control file is coherent.
if ! pg_controldata "$RESTORE_PATH" | grep -q "Database cluster state"; then
  log "FAIL: pg_controldata could not read the restored cluster"
  exit 1
fi

# Start the restored cluster on a scratch port; -w waits for it to finish recovery.
pg_ctl -D "$RESTORE_PATH" -o "-p $DRILL_PORT" -w -t 120 start

# Sanity #2: it answers queries and has left recovery (was promoted / recovered).
if ! psql -p "$DRILL_PORT" -d postgres -tAc 'SELECT 1' | grep -q '^1$'; then
  log "FAIL: restored cluster did not answer 'SELECT 1'"
  exit 1
fi
recovering="$(psql -p "$DRILL_PORT" -d postgres -tAc 'SELECT pg_is_in_recovery()')"
if [ "$recovering" != "f" ]; then
  log "FAIL: restored cluster is still in recovery (pg_is_in_recovery=$recovering)"
  exit 1
fi

log "PASS: PITR restore verified (stanza='$STANZA', target=${TARGET_TIME:-latest})"
