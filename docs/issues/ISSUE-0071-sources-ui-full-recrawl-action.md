# ISSUE-0071: Full re-crawl action for sources in the admin UI

**Type:** Feature · **Status:** Done · **Story:** STORY-11.2 (follow-up) · **Traces:** FR-ADM-01, ADR-0075

## Summary
The Sources table already had an incremental **Sync** action, but the backend `full` sync flag
(`syncSource(..., full)`; `internal/cp/sources` `Full bool`) was never exposed — the UI hardcoded
`full=false`. Add a **Full re-crawl** row action (`full=true`) for non-`upload` sources. It re-fetches
every page, ignoring conditional-fetch caching, so an operator can recover content that a truncated or
timed-out crawl soft-deleted (the manual.tourpaq.com case: a crawl that hit the 30-minute
`syncJobTimeout` after 153 of ~960 pages left the rest soft-deleted by full-sync reconciliation).

## Scope
- `web/components/SourcesTable.tsx`: `onFullSync` prop + a "Full re-crawl" `RowAction`, shown only when
  `kind !== "upload"` (upload sources have nothing to crawl).
- `web/app/admin/sources/page.tsx`: a `fullSync` mutation calling `syncSource(..., true)`, wired behind
  a `window.confirm` (a full re-crawl is heavier than a normal sync).

## Out of scope
- The crawl-slowness / timeout root cause and the "a truncated crawl should not full-sync delete"
  correctness concern (see the investigation notes) are separate backend issues.

## Tests / runnable checks
- `web/components/SourcesTable.test.tsx`: Full re-crawl shows for a `web_crawl` source and wires the
  callback; hidden for `upload`. `cd web && npx vitest run`: **PASS** (100); `npm run build` +
  `npm run lint`: clean.
