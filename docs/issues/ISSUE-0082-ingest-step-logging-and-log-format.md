# ISSUE-0082: Ingest pipeline step logging and a human-readable log format

**Type:** Chore (observability) · **Status:** Done · **Priority:** Medium · **Traces:** SPEC-10 §1, ADR-0014

## Summary
A sync/ingest job that appeared "stuck or not requesting data" gave no way to see where it was: the
per-document pipeline in `internal/ingest/sink` (parse → hash → chunk → embed → commit) logged nothing,
and a per-document parse/embed failure was only counted in `Stats` — `Sink.Put` returns `nil` for a
recorded failure, so nothing reached the logs. Only infrastructure errors (e.g. the ISSUE-0076
`content_hash` one) surfaced, via the connector's own "sink rejected document" line.

## What was built
- **Step logs in `Sink.Put`** (`internal/ingest/sink/sink.go`), at Debug, one line per stage with the
  document `external_id`, counts and timings — never document content (C-3):
  `document received` → `parsed` (parser, chars) → `unchanged, skipped` | `chunked` (n) →
  `embedding chunks` (to_embed, reused, provider, model) → `embedded chunks` (tokens, **duration_ms**)
  → `committed document`. The embed pair brackets the provider call, so a stalled embedding request is
  visible as an `embedding chunks` line with no matching `embedded chunks`.
- **Per-document failures now log at Warn** (`recordFailure`) — previously invisible outside `Stats`.
- **Wiring:** `sink.Config.Log`, passed from the sync worker and the upload ingestor (`ingestdoc`);
  nil falls back to `slog.Default()`.

## Log format (the "clearer logs" ask)
- `obs.NewLogger(service, level, format, w)`: `format="text"`/`"console"` emits human-readable
  key=value lines (slog `TextHandler`); anything else stays JSON. `obs.Logger` delegates to the JSON
  form, so all existing callers and tests are unchanged.
- Config: `LOG_FORMAT` (default `json`), threaded through `ObsSettings` to `ragctl serve` / `work`.
- **zerolog was evaluated and rejected:** it requires Go ≥ 1.23, but `go.mod` is deliberately pinned to
  `go 1.22` (ADR-0014) for OTel/AWS-SDK/pgx/grpc compatibility and the vuln-gate strategy. Adopting it
  would break that pin (a dedicated Go-pin-advance story per `docs/dependency-policy.md`) and pull ~5
  modules. slog's built-in `TextHandler` delivers readable console output with zero new dependencies
  and no pin change. If colored/zerolog output is later wanted, do it under the Go-pin-advance story.

## Usage
Set `LOG_LEVEL=debug` to see the step trace and `LOG_FORMAT=text` for readable lines (both set in the
local `.env`). Restart the worker to apply.

## Tests
- `internal/obs` unit `TestNewLoggerTextFormatIsHumanReadable`: `text`/`console` emit key=value (not
  JSON) with the service field; `json`/`""` stay JSON.
- Existing sink/ingestdoc/worker unit tests stay green (the logger defaults to `slog.Default()` when
  unset).

## Related
ADR-0014 (Go 1.22 pin), `docs/dependency-policy.md`, ISSUE-0076 (the drift that first surfaced the
gap), SPEC-10 §1 (no content at info level).
