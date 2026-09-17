# ISSUE-0070: Per-tenant LLM provider/model selection in the admin settings UI

**Type:** Feature · **Status:** Done · **Story:** STORY-11.5 (follow-up) · **Traces:** SPEC-02 §5, ADR-0075

## Summary
The tenant Settings page already edited `settings.llm.provider` and `settings.llm.model`, but as
free-text inputs. This makes the query model a first-class **selection** per tenant: the provider is a
dropdown over the tenant's `providers_allowed`, and the model is a datalist over
`settings.llm.models_allowed`. Wildcard entries (e.g. `gpt-*`) are excluded from the concrete
suggestions but the model field stays free-text, so a wildcard-allowed provider can still take a
specific model name. No backend change — the `settings.llm` shape and the PATCH validation
(providers_allowed / models_allowed) already existed.

## Scope
- `web/components/SettingsForm.tsx`: add `SelectRow` (provider, from `providers_allowed`) and
  `DatalistRow` (model, suggestions from concrete `models_allowed`); replace the two LLM `TextRow`s.

## Out of scope
- Configuring the platform provider credentials that generation needs (`ANTHROPIC_API_KEY` /
  `OPENAI_API_KEY` + `OPENAI_BASE_URL`). Selecting a model only routes to a provider the platform must
  already be able to reach; a tenant on a provider with no configured key still fails generation. That
  is an operator/env concern, not a settings-UI one.

## Tests / runnable checks
- `web/components/SettingsForm.test.tsx`: provider renders as a `<select>` over `providers_allowed`;
  the model datalist offers concrete `models_allowed` (wildcards excluded) and a picked model is sent
  in the PATCH. `cd web && npx vitest run SettingsForm`: **PASS** (10); `npm run build` + `npm run
  lint`: clean.
