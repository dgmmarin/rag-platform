# ISSUE-0008: Parsing sidecar (Python) and Go client

**Type:** Feature · **Status:** Done · **Story:** STORY-05.3 · **Traces:** FR-ING-11, FR-SRC-02, SPEC-05 §2, SPEC-05 §8, ADR-0006, ADR-0035

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue file records STORY-05.3 for traceability; the backlog
> story remains the authoritative work item.

## Summary
Turn the health-only parsing-sidecar stub (STORY-01.2) into the real service
(ADR-0006, SPEC-05 §2): `POST /parse` extracts PDF/DOCX/PPTX/XLSX into the same
`Normalised{title, blocks}` shape the Go parsers emit (STORY-05.2), and add the Go
client the ingest worker uses to call it — with the SPEC-05 §2 timeout, retries,
Retry-After handling and tracing.

## Scope
- Python service (`services/parser/`): `POST /parse` (multipart `file` + `mime`) →
  `{title, blocks:[{type, level?, text?, rows?}]}`; keep `GET /healthz`; container
  image; fixtures for 6 documents across the 4 formats.
- Go client (`internal/ingest/sidecar/`): `Parse(ctx, filename, mime, data)
  (parse.Normalised, error)` with timeout, retries, tracing; `PARSER_URL` config.

## Resolution
- `services/parser/parsers.py`: pure per-format extractors — PDF (PyMuPDF: font-size
  heading heuristic + `find_tables`), DOCX (python-docx, body walked in document
  order to interleave paragraphs/tables), PPTX (python-pptx: slide titles, text,
  tables), XLSX (openpyxl: per-sheet heading + table). A `markdown_table` mirroring
  the Go `markdownTable`. `MIME → extractor` `DISPATCH`.
- `services/parser/app.py`: Flask app — `/healthz`, `/parse` (415 unsupported, 422
  parse failure, 413 too large via `MAX_CONTENT_LENGTH`, 400 missing parts). Served
  by gunicorn (`Dockerfile`, `requirements.txt`); non-root; 120 s worker timeout.
- `services/parser/gen_fixtures.py` generates the six committed fixtures
  (`testdata/`): report.pdf, article.pdf (2 pages), memo.docx, letter.docx,
  deck.pptx, budget.xlsx (2 sheets). `test_parsers.py` (pytest): every fixture →
  headings (+ tables for the office formats), the markdown/rows contract, and the
  HTTP surface incl. error codes.
- `internal/ingest/sidecar/client.go`: `Client.Parse`/`Healthz`. Builds the
  multipart body, POSTs to `{PARSER_URL}/parse`, decodes into `parse.Normalised`.
  120 s timeout; retries transport errors/429/5xx with capped exponential backoff
  honouring `Retry-After` and cancellable by context; 415→`ErrUnsupportedFormat`,
  422→`ErrParseFailed` (both terminal). OpenTelemetry span + W3C trace-context
  injection via the installed global propagator (no `otelhttp` dependency).
- `internal/config`: `ParserURL` from `PARSER_URL` (compose already sets
  `http://parser:8081`).
- `mise-tasks/test-parser` + a CI `parser` job run the pytest suite (ADR-0014
  parity); `.gitignore` covers the local venv/caches. No schema/OpenAPI/migration
  change.

## Verification
- `mise run test-parser` (pytest): 13 passed — fixtures parse to headings/tables,
  markdown==markdown_table(rows), `/parse` 200/400/415/422, `/healthz`.
- `go test ./internal/ingest/sidecar ./internal/config`: green — success/decoding,
  415/422 terminal (1 call, no retry), retry-then-succeed (3 calls), give-up,
  Retry-After cut short by a short context, trace-context injection, health.
- Parser output eyeballed (report.pdf → H1/H2 + paragraphs; memo.docx → heading +
  GFM table + rows; budget.xlsx → per-sheet heading + table).
- `gofmt` clean; `golangci-lint` 0 issues on the new Go packages.
- Container image: `docker build services/parser` installs every runtime dep as a
  manylinux wheel (Flask, gunicorn, PyMuPDF, python-docx/pptx, openpyxl — build log
  confirms `Successfully installed …`). A clean tagged export could not be confirmed
  locally (the docker daemon in this dev environment wedged mid-run); the CI
  `integration` job builds the sidecar via `docker compose` on a clean runner.

## Notes / not in scope
- Block shape uses SPEC-05 §2 `rows` (not ADR-0006's earlier `table`), so the JSON
  decodes straight into `parse.Normalised`; reconciled in ADR-0035.
- Ceiling (ponytail, ADR-0035): PDF heading detection is font-size ranking and PDF
  tables are appended per page — a PDF heading may be mis-levelled or a table
  slightly out of order, never lost. DOCX/PPTX/XLSX are deterministic.
- The sink (STORY-05.6) routes `parse.ErrUnsupportedMIME` from the Go registry to
  this client and records `metadata.parse_error` on `ErrParseFailed`; the routing
  seam exists but the orchestration is that story's.
- Pre-existing local check noise (ISSUE-0003/0006): env-sensitive `internal/cli`
  `*RequiresURL` tests fail under mise `.env` injection; unrelated to this work.
