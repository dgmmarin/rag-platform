# ISSUE-0064: Session tenant-scoped settings/members/API-keys API + admin UI pages (STORY-11.5)

**Type:** Feature · **Status:** In progress · **Story:** STORY-11.5 · **Traces:** SPEC-02 §2/§5, ADR-0075, ADR-0021, ADR-0022

> The *what* lives in `docs/backlog/`; the *why* in the ADRs above. Plan:
> `docs/superpowers/plans/2026-09-16-epic11-story-11.5-members-keys-settings.md`.

## Summary
STORY-11.5 gives the session admin UI three tenant-scoped management surfaces — **members**
(roster, add-by-email, change role, remove), **API keys** (list, mint with a one-time secret
reveal, revoke) and **settings** (SPEC-02 §5 document) — over session-authenticated routes behind
`RequireTenantAccess`, reusing the EXISTING `auth.MembershipService`, `auth.APIKeyService` and
`tenants.SettingsService` verbatim (ADR-0075). The only new server logic is a `UserByEmail` lookup
(add-by-email resolves to an existing user; no invite/email flow in this story) plus thin HTTP
handlers.

## Scope (see plan for detail)
- **Task 1 (server):** `MembershipHandlers` + `APIKeyHandlers`, `UserByEmail`, mount
  settings/members/api-keys behind `RequireSession -> RequireTenantAccess` (reads `PermQuery`;
  member/key writes `PermManageMembers`; settings PATCH `PermChangeSettings`); CSRF on mutations.
- **Task 2–4 (web):** settings page, members page, API keys page.
- **Task 5:** close-out.

## Tests / runnable checks
- (filled as tasks land)
