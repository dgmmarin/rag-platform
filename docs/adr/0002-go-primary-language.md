# ADR-0002: Go as the primary implementation language

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** C-2, NFR-PERF-04, NFR-SCAL-03, NFR-MNT-01

## Context
The system is dominated by infrastructure work: HTTP API, crawlers, API connectors, job workers, database access, provider clients. ML-heavy work (document parsing, model inference) is delegated to external services. The team has Go expertise.

## Options
1. Python end to end with LlamaIndex/LangChain.
2. Go end to end.
3. Go for services; Python sidecar for parsing where Go libraries are weak.

## Decision
Option 3. Go (1.22+) for the control plane, API, connectors, crawler, ingestion orchestration, retrieval and CLI. A small Python sidecar for document parsing (ADR-0006). Embeddings and generation are called over HTTP from Go.

## Consequences
- Single static binary with subcommands (`serve`, `work`, `migrate`, `enroll`); small images; straightforward horizontal scaling.
- Concurrency for crawling and batch embedding is native.
- No RAG framework: connector and retrieval plumbing is written in-house against small interfaces (SPEC-04, SPEC-06). More code owned, less abstraction fought.
- Experimentation with retrieval strategies is slower than in Python notebooks; mitigated by the evaluation harness (FR-ADM-04) and a scripted eval workflow.
- Key libraries: `pgx`/`pgvector-go`, `river` (jobs), `colly` (crawl), `tiktoken-go`, `goose` (migrations), `chi` or `net/http` (API), `otel`.
