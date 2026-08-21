# ADR-0007: Hybrid retrieval with reciprocal rank fusion, optional rerank

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** FR-RET-01, FR-RET-03, FR-RET-05

## Context
Company and product questions contain exact identifiers (SKUs, error codes, product names) where lexical matching outperforms embeddings, and paraphrased questions where embeddings outperform lexical. Either alone under-performs.

## Decision
Retrieve top-K (default 40) from vector similarity and top-K from full-text search, merge with reciprocal rank fusion (k=60), apply metadata filters in SQL before ranking, then optionally rerank the top-N (default 20) with a cross-encoder or LLM reranker to produce the final top-k (default 8) passed to generation. A relevance floor on the fused/reranked score triggers the "no sufficiently relevant content" response.

## Consequences
- Robust across question types with one code path; each stage is toggleable per tenant (tenants.settings).
- Rerankers add 100–300 ms; enabled per tenant based on eval results.
- Eval harness tracks recall@k and grounded-rate per configuration to justify parameter changes.
