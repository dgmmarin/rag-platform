# ADR-0046: Conditional fetch and change detection — conditional GET over HEAD, content-hash fallback, and the deletion-detection reconciliation

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** FR-ING-02, SPEC-04 §2, C-1, C-3 · **Decisions:** ADR-0003, ADR-0038, ADR-0043

## Context
STORY-07.1 (ADR-0043) built the web-crawl core and *persisted* each fetched page's
ETag, Last-Modified and content hash into `crawl_pages`, but deliberately left the
conditional-request / 304-skip optimisation as a clean seam for STORY-07.4. It also
flagged a ceiling: the resume-skip (already-fetched URLs are not refetched) interacts
with the ingestion sink's full-sync deletion detection (unseen ⇒ soft-delete,
ADR-0038), and reconciling that "cheap re-see" was deferred here.

STORY-07.4 (FR-ING-02) closes the seam: unchanged pages must cost a 304 and no parse,
and the content hash must catch changes when a server sends no validators. The change
is localised to the crawler's fetch/process path and the `PageStore` read of the
prior validators — no change to the egress guard (07.2), the extractor (07.3), the
ingestion sink, or the `connector.Sink` interface.

## Options and decisions

- **Conditional GET (→ 304), not HEAD.** The AC phrases the fast path as "a HEAD/304".
  A conditional `GET` with `If-None-Match`/`If-Modified-Since` is the canonical HTTP
  mechanism and is strictly better than a separate `HEAD` then `GET`:
  - it is **one** round trip, and returns **no body** when the page is unchanged
    (a 304 has empty content) — so an unchanged page costs exactly what a HEAD would,
    while a HEAD *always* needs a second GET when the page *has* changed (two round
    trips on every change);
  - `HEAD` responses need not carry the same validators/headers a `GET` would, and
    many origins/CDNs handle conditional GET far more reliably than HEAD;
  - it composes with the existing single-`GET` fetch path with no second request type.
  There is no spec-driven reason to prefer HEAD, so we use conditional GET. `ETag`
  is sent as `If-None-Match` (primary); `Last-Modified` as `If-Modified-Since`
  (secondary). Both are sent when both are stored — the server picks.

- **Content-hash fallback for the no-validator case.** Many servers send neither
  ETag nor Last-Modified, so a 304 is impossible and a 200 is unavoidable. On a 200
  the crawler computes the SHA-256 of the fetched bytes (07.1 already stores this)
  and compares it to the stored hash; identical bytes ⇒ unchanged ⇒ skip parse/emit.
  This is the raw-body hash (a cheap pre-parse gate); it is distinct from and
  complementary to the ingestion sink's *normalised-markdown* content hash (SPEC-05
  §1) — the crawler avoids the parse entirely, the sink avoids the embed.

- **Re-visit model — incremental re-sees, full keeps the 07.1 skip.** Conditional
  fetch only matters when a previously-fetched page is *re-visited*; 07.1's `run`
  unconditionally skipped fetched URLs (correct for a crashed-crawl resume, wrong for
  a scheduled re-sync, which would then do nothing). 07.4 splits on `SyncRun.Full`:
  - **incremental** (`Full == false`): previously-fetched pages are re-queued at
    their recorded depth for a **conditional** re-visit — a 304 or an identical hash
    costs no parse/emit; only changed pages are re-emitted, and a changed page's new
    links re-enter the frontier (an unchanged page's links are unchanged by
    definition, already in `crawl_pages`, so skipping link extraction is safe);
  - **full** (`Full == true`): the STORY-07.1 resume-skip is preserved unchanged.
  All existing unit and e2e tests run with `Full: true` and are unaffected.

- **Deletion-detection reconciliation — no false deletions.** The ingestion sink
  soft-deletes, on `Complete`, any document not `Put` this run (ADR-0038); a 304 /
  hash-skip page is intentionally *not* `Put` (that is the whole optimisation).
  Feeding conditional-crawl output to a **Full**-mode deletion sink would therefore
  wrongly delete every unchanged page. The reconciliation:
  - conditional skip runs **only on incremental syncs**, where the sink's `Complete`
    is a **no-op** (`sink.Incremental`, SPEC-05 §1/§5) — so an unchanged, un-emitted
    page can never be soft-deleted. This is the "moot in Incremental mode" resolution
    the story anticipated, and it mirrors the platform's own API-connector pattern
    (SPEC-04 §4: incremental syncs skip unchanged, a periodic **full** sync
    re-enumerates for deletion detection).
  - the unchanged page is still **marked seen** in `crawl_pages` (`last_fetched_at`
    is bumped on every 304 / hash-skip), so the crawl-state bookkeeping records the
    re-see even though the document sink does not.
  - **Ceiling / upgrade path.** ADR-0043 envisioned using a cheap 304 re-see *during
    a full sync* so deletion detection could re-enumerate every page without
    re-downloading bodies. That additionally requires signalling "seen, unchanged"
    to the ingestion sink **without** a re-`Put` — i.e. a new `connector.Sink` method
    (e.g. `Seen(externalID)`), an interface change owned by the worker/adapter layer
    (EPIC-09), out of scope for this localised story. Until then, deletion detection
    for web crawls is the periodic full re-enumeration (which re-emits what it
    fetches), exactly as the API connector does. Recorded here so the seam is explicit.

- **Store-update semantics — carry prior state forward explicitly.** On a 304 /
  hash-skip, `markUnchanged` upserts the page with the **prior** status and content
  hash carried forward (refreshing ETag/Last-Modified only if the response supplied
  new ones) and `last_fetched_at = now()`. This is done by passing the full prior
  `Page`, not by relying on the `crawl_pages` SQL `coalesce`, so the in-memory test
  store and the `tenant.DB` store behave identically and the intent is explicit at
  the call site. The persistence is best-effort (logged, not fatal) — a hiccup only
  degrades the *next* crawl's conditional efficiency and must not abort a live crawl,
  unlike a first-fetch state write on which resumability depends.

## Consequences
- **No schema change:** `crawl_pages` already carries `etag`, `last_modified`,
  `content_hash`, `last_fetched_at` (SPEC-03 §2); the drift guard stays green,
  `api/openapi.yaml` is unchanged (no request/response schema moved).
- **No new dependency:** the change is `crypto/sha256`, `bytes` and `net/http`
  conditional headers over the existing fetch path.
- **Constraints upheld:** all `crawl_pages` I/O stays inside the `tenant.DB`
  `PageStore` adapter (ADR-0003, C-3); no `tenant_id` column, no cross-DB FK (C-1,
  SPEC-03 §2 inv. 4); no secret is logged; no document content is logged (identity
  only, at debug).
- **Observability:** an unchanged page is counted in `Stats.DocsSeen` (and its bytes
  for a hash-skip; zero for a 304) but never in `DocsChanged` — so a re-sync's
  "seen vs changed" split is honest (SPEC-10).
- The full-sync cheap-304-re-see remains available to EPIC-09 once a sink "mark seen"
  signal exists; the crawler already sends conditional headers and records the re-see.
