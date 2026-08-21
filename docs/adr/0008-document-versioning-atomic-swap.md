# ADR-0008: Document versions with atomic current-version swap

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-ING-02, FR-ING-07, NFR-REL-02

## Context
A sync re-fetches documents that may or may not have changed. Re-chunking and re-embedding in place would expose partially updated documents to queries and waste provider calls on unchanged content.

## Decision
`documents` holds identity and a pointer `current_version`. `document_versions` holds immutable normalised content keyed by content hash. A sync: (1) fetches, normalises, hashes; (2) if hash equals current version, only updates `last_seen_at`; (3) otherwise inserts a new version and its chunks, then in one transaction flips `current_version`. Retrieval reads through the `live_chunks` view. Documents not seen in a completed full sync are marked deleted. Old versions and their chunks are garbage-collected after a retention window.

## Consequences
- Unchanged documents cost one hash comparison, no embedding calls.
- Queries never see a half-built document.
- Rollback to a previous version is a pointer change.
- Storage grows with versions; GC job required (SPEC-08).
