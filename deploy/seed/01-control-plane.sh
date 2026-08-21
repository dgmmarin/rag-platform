#!/usr/bin/env bash
# Seed script for the local stack (STORY-01.2).
#
# Runs once on first Postgres init (mounted into /docker-entrypoint-initdb.d/).
# Ensures the control-plane database exists and the pgvector extension is
# installed. The official Postgres image already creates ${POSTGRES_DB}, but we
# create it explicitly and idempotently so the seed is self-contained and works
# even if POSTGRES_DB is pointed elsewhere.
#
# It intentionally does NOT apply the full control_plane.sql schema: that is
# owned by goose migrations (STORY-01.5, `ragctl migrate control`). Seeding only
# guarantees the database + extension the app connects to.
set -euo pipefail

DB="${POSTGRES_DB:-control_plane}"

# Create the control-plane database if it is not the one we are already
# connected to (i.e. POSTGRES_DB pointed somewhere else).
if [ "$DB" != "control_plane" ]; then
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$DB" <<-SQL
	SELECT 'CREATE DATABASE control_plane'
	WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'control_plane')\gexec
SQL
fi

# Install pgvector in the control-plane database. Retrieval (ADR-0004) lives in
# the tenant databases, but installing here keeps the local default sane and
# matches what migrations expect.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname control_plane <<-SQL
	CREATE EXTENSION IF NOT EXISTS vector;
	CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL

echo "seed: control_plane ready (pgvector, pgcrypto installed)"
