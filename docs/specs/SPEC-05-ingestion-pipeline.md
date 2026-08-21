# SPEC-05: Ingestion pipeline

**Implements:** FR-ING-01..11 · **Decisions:** ADR-0006, ADR-0008

## 1. Flow per document
```
Connector.Sync ─► Sink.Put(doc)
                    │
                    ├─ parse        (Go parsers or Python sidecar) ─► Normalised{Title, Blocks}
                    ├─ normalise    (markdown text, heading tree, tables as markdown)
                    ├─ hash         (sha256 of normalised text)
                    ├─ compare      (== current version hash? touch last_seen_at, return changed=false)
                    ├─ insert version
                    ├─ chunk        (structure-aware, target/overlap from settings)
                    ├─ embed        (batched, async via embedding queue)
                    └─ commit       (insert chunks; flip current_version in one tx)
```
```mermaid
flowchart TD
    sync[Connector.Sync] --> put["Sink.Put(doc)"]
    put --> parse{parse}
    parse -->|"pdf / docx / pptx / xlsx"| sidecar[Python sidecar<br/>POST /parse]
    parse -->|"html / md / csv / json"| goparse[Go parsers]
    sidecar --> norm[normalise<br/>markdown, heading tree, tables]
    goparse --> norm
    norm --> hash[hash<br/>sha256 of normalised text]
    hash --> cmp{same as current<br/>version hash?}
    cmp -->|yes| touch[touch last_seen_at<br/>changed=false] --> done([done])
    cmp -->|no| ver[insert document_version]
    ver --> chunk[chunk<br/>structure-aware, target/overlap]
    chunk --> embed[embed<br/>batched, async queue]
    embed --> commit[commit tx<br/>insert chunks +<br/>flip current_version]
    commit --> done

    complete["Sink.Complete (full sync)"] -.-> softdel["docs with last_seen_at &lt; run.started_at<br/>set status='deleted'"]
```

`Sink.Complete` (full sync only): documents of the source with `last_seen_at < run.started_at` → `status='deleted'`.

## 2. Parsers
| MIME | Parser |
|---|---|
| text/html | Go: readability + html-to-markdown |
| text/markdown, text/plain | Go passthrough, heading detection |
| text/csv, application/json | Go: row/record → markdown table or key-value text |
| application/pdf, docx, pptx, xlsx | Python sidecar `POST /parse` |
Sidecar response: `{title, blocks:[{type:"heading|paragraph|table|list|code", level, text, rows}]}`. Sidecar timeout 120 s; failures mark the document with `metadata.parse_error` and do not fail the whole sync.

## 3. Chunking
- Walk blocks maintaining `heading_path`.
- Accumulate paragraphs until `target_tokens` (default 512); never split a table row or code block unless it alone exceeds 2× target.
- Overlap: carry the last `overlap_tokens` (64) of the previous chunk.
- Prepend a context line to the embedded text (not stored content): `"{title} > {heading_path joined}"` — improves retrieval of short chunks.
- Token counting via tiktoken `cl100k_base` (approximation is acceptable across providers).

## 4. Embedding
- Interface `Embedder.Embed(ctx, []string) ([][]float32, error)`; providers: OpenAI, Voyage, Cohere, TEI (self-hosted).
- Batches of ≤ 96 texts / ≤ 100k tokens; bounded concurrency per tenant (default 4 batches in flight).
- Retries with exponential backoff on 429/5xx; honours `Retry-After`; circuit breaker per provider.
- Token usage recorded into `jobs.stats` and `usage_daily`.

## 5. Commit semantics
Per document, one transaction: insert `document_versions`, insert all `chunks`, `UPDATE documents SET current_version=$1, last_seen_at=now(), status='active'`. If embedding fails the transaction is not opened; the document keeps its previous version and the job records the error.

## 6. Job stats (`jobs.stats`)
`docs_seen, docs_changed, docs_unchanged, docs_deleted, docs_failed, chunks_written, embed_tokens, bytes_fetched, duration_ms, errors:[{external_id, msg}] (capped at 100)`.

## 7. Reindex job
Iterates live versions in `document_id` order, re-chunks if chunking settings changed, embeds with the new model into `chunks_new`, then performs the swap from SPEC-03 §5. Resumable via a cursor stored in job payload.

## 8. Failure modes and handling
| Failure | Handling |
|---|---|
| Source unreachable | job fails, retry with backoff (3 attempts), source.status=error after final failure |
| Single document parse error | recorded, skipped, job succeeds with `docs_failed>0` |
| Provider quota exhausted | job paused (snoozed) for `Retry-After`, not failed |
| Worker crash mid-sync | per-document transactions mean no partial documents; job retried; unchanged docs are skipped by hash |
