# ADR-0006: Python sidecar for document parsing

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-SRC-02, FR-ING-11

## Context
High-quality extraction from PDF and DOCX (headings, tables, reading order, OCR) is best served by Python libraries (Docling, Unstructured, PyMuPDF). Go libraries handle plain text extraction but are weak on layout and tables.

## Options
1. Go-native parsing only.
2. Python sidecar service called over HTTP by the Go worker.
3. Third-party hosted parsing API.

## Decision
Option 2. A stateless Python service exposes `POST /parse` (multipart file + mime) → JSON `{title, blocks:[{type, level, text, table}]}`. The Go worker uses it for PDF, DOCX, PPTX and XLSX. HTML, Markdown, TXT, CSV and JSON are parsed in Go.

## Consequences
- Best-in-class parsing without polluting the Go services with ML dependencies.
- One more deployable; sized and scaled independently (CPU-heavy).
- Clear interface means it can be replaced by Go code or a hosted API later without touching ingestion.
- Parser name/version is recorded on `document_versions.parser` so re-parsing can be targeted.
