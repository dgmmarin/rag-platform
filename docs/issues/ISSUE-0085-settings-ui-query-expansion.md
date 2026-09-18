# ISSUE-0085: Expose the query-expansion mode in the settings UI

**Type:** Chore (UI) · **Status:** Done · **Priority:** Medium · **Traces:** ISSUE-0084, ADR-0079, SPEC-11

## Summary
ISSUE-0084 added the `settings.expansion.mode` toggle (`off` | `hyde`), but it was only editable via the
API / DB. This adds a control to the admin settings form so an operator can turn HyDE query expansion
on or off per tenant.

## What was built
- `web/lib/settings.ts`: `Settings.expansion` and `SettingsPatch.expansion` types.
- `web/components/SettingsForm.tsx`: a "Query expansion" section with an "Expansion mode" dropdown
  (`off` / `hyde`), threaded through the draft and the change-only patch diff (mirroring the reranker /
  rewrite controls). A plain-language description explains the trade-off (better recall on wording
  mismatches, one extra LLM call per query).

## Tests
- `web/components/SettingsForm.test.tsx`: the control renders the current mode, and changing it to
  `hyde` sends `{expansion: {mode: "hyde"}}` in the patch; the settings fixture carries `expansion`.

## Related
ISSUE-0084 (HyDE query expansion), ADR-0079, the tenant settings schema (`settings.expansion`).
