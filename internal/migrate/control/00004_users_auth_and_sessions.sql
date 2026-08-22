-- +goose Up
-- ---------------------------------------------------------------------------
-- Password authentication and server-side sessions (STORY-03.1, FR-ACC-01, SPEC-09 §3)
-- ---------------------------------------------------------------------------
-- Email/password login lives entirely in the control plane (C-3): the users row
-- gains an argon2id password hash and the counters the lockout policy needs
-- (10 failures / 15 min, SPEC-09 §3). password_hash stays null for OIDC-only
-- users (STORY-03.2). Nothing here is ever logged or returned by an API.

alter table users add column if not exists password_hash text;
alter table users add column if not exists failed_login_count int not null default 0;
alter table users add column if not exists locked_until timestamptz;

-- Server-side sessions for the admin UI cookie (SPEC-02 §3, SPEC-09 §3). The
-- cookie carries a 128-bit random id; only its sha256 hash is stored here, so a
-- leaked control-plane snapshot cannot be replayed as a live session. csrf_token
-- backs the double-submit check on mutating routes. idle_expires_at enforces the
-- 12 h idle timeout and is pushed forward as the session is used.
create table sessions (
    id              uuid primary key default gen_random_uuid(),
    user_id         uuid not null references users(id) on delete cascade,
    token_hash      bytea not null unique,              -- sha256 of the 128-bit cookie id; never store plaintext
    csrf_token      text not null,                      -- double-submit token for mutating routes
    created_at      timestamptz not null default now(),
    idle_expires_at timestamptz not null,              -- 12 h idle timeout; advanced on use
    revoked_at      timestamptz                         -- set on logout; a revoked session never authenticates
);
create index on sessions (user_id);
create index on sessions (idle_expires_at) where revoked_at is null;

-- +goose Down
drop table if exists sessions;
alter table users drop column if exists locked_until;
alter table users drop column if exists failed_login_count;
alter table users drop column if exists password_hash;
