# ISSUE-0058: Admin UI OIDC browser callback (EPIC-11 follow-up)

**Type:** Feature · **Status:** Todo · **Story:** EPIC-11 follow-up · **Traces:** ADR-0020, ADR-0073, SPEC-11 §2

> Note: the *what* lives in the delivery backlog (`docs/backlog/`), the *why* in ADRs (ADR-0020, ADR-0073).

## Summary
STORY-11.1's login page linked `Sign in with OIDC` to `/bff/v1/auth/oidc/start`, but
`OIDCHandlers.Callback` (`internal/cp/auth/oidc_handlers.go`) responds with JSON `{csrf_token}` —
the same body shape as the password-login fetch client, per ADR-0020's "do not fork session
handling" — not a browser redirect. A top-level navigation through `Start` therefore round-trips
correctly through the provider but ends the flow on a raw-JSON page instead of re-entering the
Next.js SPA, so the button was removed from the login UI in the STORY-11.1 final-review fix wave
(final-review report, `web/app/admin/login/page.tsx`) rather than ship a dead end.

## Scope (to build)
- `OIDCHandlers.Callback` must, on success, `303 See Other`-redirect the browser into the admin
  UI (e.g. `/admin`) instead of returning JSON — the session cookie is already set via
  `Set-Cookie` on the same response, so the redirect target only needs to land back in the SPA
  for `GET /v1/auth/me` hydration to pick it up.
- The post-login redirect target should be configurable (a base URL / path), not hardcoded, so
  the same binary works across environments (mirrors how the OIDC provider's own redirect URIs
  are already configured per ADR-0020).
- `web/app/admin/login/page.tsx` re-enables the `Sign in with OIDC` control once the above lands,
  pointing at the BFF-proxied `/bff/v1/auth/oidc/start` as before.
- Failure paths (`ErrOIDCStateMismatch`, `ErrOIDCEmailUnverified`, `ErrOIDCUserNotProvisioned`,
  etc.) need the same treatment: redirect to a login-page error state (e.g. `/admin/login?error=...`)
  rather than a raw JSON error body, so a browser navigation never dead-ends either way.

## Out of scope
- Any change to the OIDC authorization-code+PKCE exchange itself (`OIDCService`, state/nonce
  handling) — this issue is only about how `Callback` terminates the HTTP response for a browser
  client.

## Notes
- Traced against SPEC-11 §2 ("The OIDC button navigates to the Next-proxied
  `/v1/auth/oidc/start`; the provider `redirect_uri` targets the Next origin.") — that document
  already assumes the end-to-end browser flow this issue delivers; STORY-11.1 shipped the shell
  without it.
