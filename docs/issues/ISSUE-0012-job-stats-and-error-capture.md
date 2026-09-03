# ISSUE-0012: Job stats and error capture

**Type:** Feature · **Status:** Done · **Story:** STORY-05.7 · **Traces:** FR-ING-10, SPEC-05 §6, ADR-0038

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.7 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Finalises the in-memory ingestion `Stats` value STORY-05.6 (ADR-0038) left open:
caps `stats.errors` at 100 per-document `{external_id, msg}` entries and confirms
the marshalled shape matches SPEC-05 §6 exactly. No `jobs`/`usage_daily` write and
no worker — that stays STORY-09.1.

## Scope
- `internal/ingest/sink` only. No new package, dependency, schema, migration or
  OpenAPI change.
- The 100-error cap in the error-recording path; the SPEC-05 §6 JSON shape
  guarantee on the completed `Stats` value.
- Not in scope: persisting `jobs.stats`/`usage_daily`, the River worker (STORY-09.1),
  reindex (05.8), GC (05.9).

## Resolution
- **Cap (SPEC-05 §6).** `Sink.recordFailure` keeps at most the first
  `maxDocErrors = 100` `{external_id, msg}` entries — a single `len(...) < 100`
  guard in the one error-recording path — while `DocsFailed` always increments,
  including past the cap, so the count is honest even when the list is truncated.
  This retires the STORY-05.6 ponytail ceiling (ADR-0038: "errors left uncapped
  here").
- **Shape (SPEC-05 §6).** The `Stats` JSON tags already matched the jobs.stats
  shape (`docs_seen/changed/unchanged/deleted/failed`, `chunks_written`,
  `embed_tokens`, `bytes_fetched`, `duration_ms`, `errors:[{external_id, msg}]`);
  `duration_ms` is filled from the run clock in `Stats()`. `Stats()` now also
  normalises `Errors` to a non-nil slice so a clean run marshals `errors` as an
  empty list (`[]`), never `null` — SPEC-05 §6 specifies a list.

## Verification
- TDD: three tests added to `internal/ingest/sink/sink_test.go` and watched fail
  first — `TestErrorsCappedAtHundred` (150 failing docs ⇒ `len(errors)==100` but
  `docs_failed==150`, first 100 kept) and `TestStatsEmptyErrorsMarshalAsList`
  (`errors:null` before the fix) went red for the right reasons; then the cap +
  `Stats()` normalisation made them green. `TestStatsMarshalMatchesSpecShape`
  asserts the marshalled object carries exactly the 10 SPEC-05 §6 keys and that
  `errors` is a list of objects with exactly `external_id` and `msg`.
- `gofmt -l internal/ingest/sink/` clean; `go vet ./internal/ingest/sink/` clean;
  `go test -count=1 ./internal/ingest/sink/` green and `-race` clean; pinned
  `golangci-lint v2.13.1` **0 issues** on `internal/ingest/sink/...` (switches to
  go1.26.8 for the lint run).

## Notes / not in scope
- No new ADR: the 100-cap and list-shape are the realisation of the SPEC-05 §6
  contract and the ADR-0038 upgrade path, not a new architectural decision. ADR-0038
  remains the stats design of record.
- No schema/migration/OpenAPI change: `jobs.stats` is an existing JSONB blob written
  by STORY-05.7's downstream worker (STORY-09.1), not here.
- Coverage: `internal/ingest/sink` sits under `internal/ingest`, which reports SKIP
  against the gate (matches the path exactly, has no direct Go files).
- The pre-existing env-sensitive `internal/cli` `*RequiresURL` failures under mise
  `.env` injection are the documented ISSUE-0003/0006 noise, unrelated here.
