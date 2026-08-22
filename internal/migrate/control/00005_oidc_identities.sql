-- +goose Up
-- ---------------------------------------------------------------------------
-- OIDC login: external identity links and email-verification marker
-- (STORY-03.2, FR-ACC-01, SPEC-02 §3, SPEC-09 §3)
-- ---------------------------------------------------------------------------
-- OIDC lives entirely in the control plane (C-3). A user may hold several
-- external identities (one per issuer), so the (issuer, subject) mapping is a
-- separate table rather than the single users.external_id column, which cannot
-- distinguish issuers or index the pair (ADR-0020). users.external_id is retained
-- as an informational copy of the most-recent subject. email_verified records
-- whether the control-plane email is confirmed; JIT/link only ever attach an OIDC
-- identity to a user whose provider-asserted email_verified claim is true, so an
-- unverified email can never take over an existing account.

alter table users add column if not exists email_verified boolean not null default false;

create table user_identities (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references users(id) on delete cascade,
    issuer      text not null,                     -- OIDC issuer (iss); part of the deployment allowlist
    subject     text not null,                     -- OIDC subject (sub); stable per user per issuer
    created_at  timestamptz not null default now(),
    unique (issuer, subject)                        -- one user per (issuer, subject)
);
create index on user_identities (user_id);

-- +goose Down
drop table if exists user_identities;
alter table users drop column if exists email_verified;
