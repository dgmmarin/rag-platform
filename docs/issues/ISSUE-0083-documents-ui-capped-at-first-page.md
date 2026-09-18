# ISSUE-0083: Documents admin page shows only the first server page (50), not the full corpus

**Type:** Bug · **Status:** Done · **Priority:** High · **Traces:** ISSUE-0066, SPEC-11

## Summary
The admin Documents page reported "50 in total" for a tenant that actually held 962 documents, and there
was no way to reach the rest. `useDocuments` fetched a single list call with no limit, so the server
returned its default page (`defaultListLimit = 50`) plus a `next_cursor`, and the page then paginated
**client-side** over just those 50 (`usePagination(data.items)`). The `next_cursor` was never followed,
so 912 documents were unreachable and the total was wrong.

## Root cause
`usePagination` is client-side and assumes the list query returned the whole (bounded) dataset. That
holds for small tenants but not for a crawled corpus. The documents list query fetched only page one.

## Fix
- `web/lib/documents.ts`: added `listAllDocuments`, which follows `next_cursor` (requesting the server
  max, 200, per round trip) and aggregates every page; `useDocuments` now calls it. The client-side
  `usePagination` then pages over the full set and shows the true total.
- ponytail: capped at 100 pages (20k documents) so a pathological corpus cannot loop unbounded. A
  tenant beyond that should move the table to true server-side cursor paging (a follow-up), rather than
  lifting the cap.

## Tests
- `web/lib/documents.test.ts`: `listAllDocuments` follows `next_cursor` across pages and aggregates
  (asserts `limit=200` on the first call and `cursor=` on the second), and stops after one page when
  there is no cursor.

## Related
ISSUE-0066 (session-tenant-scoped documents API + admin UI), the same client-side `usePagination`
pattern used by the other admin tables (sources/jobs/members) — they share this ceiling and should
adopt the same fix if a tenant's list grows past one server page.
