# Chunk-level drift detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On re-ingesting a changed document, embed only the chunks whose content is new and reuse the existing vector for any chunk with the same `(content_hash, embedding_model)` tenant-wide.

**Architecture:** Add a per-chunk `content_hash` column to `chunks`. A new `embedcache` service does one thing — look up existing embeddings by `(content_hash, model)`. The sink's changed-document path hashes each chunk, asks the cache which are already known, embeds only the misses, and commits reused + new vectors in the same single transaction.

**Tech Stack:** Go 1.22, pgx v5, PostgreSQL (VectorChord `vchordrq`), River.

**Spec:** `docs/superpowers/specs/2026-09-17-chunk-level-drift-detection-design.md`

## Global Constraints

- Tenant content is reached ONLY through `*tenant.DB` from the resolver (ADR-0003). The `embedcache` takes a `*tenant.DB`, never a raw pool.
- The changed-document commit stays ONE transaction (ADR-0008, SPEC-05 §5). Reuse changes what is embedded, never the commit's atomicity.
- `content_hash` is `sha256(chunk.EmbedText)` — the exact bytes sent to the embedder, so hash and vector always correspond.
- No new migration file: the schema edit rides `internal/migrate/tenant/00001_initial_schema.sql` (operator wipes + re-provisions, per ADR-0076).
- gofmt clean; every task ends green (`go build ./...` + the task's tests).

---

### Task 1: Add `content_hash` to the chunks schema

**Files:**
- Modify: `internal/migrate/tenant/00001_initial_schema.sql` (chunks table + indexes)

**Interfaces:**
- Produces: a `chunks.content_hash bytea not null` column and index `(content_hash, embedding_model)` that Tasks 3–4 write/read.

- [ ] **Step 1: Add the column and index**

In the `create table chunks (...)` block, add after `embedding_model text not null,`:
```sql
    content_hash    bytea not null,                     -- sha256(embed-text); chunk-level drift reuse
```
After the existing `create index chunks_embedding_idx ... vchordrq ...` block, add:
```sql
create index on chunks (content_hash, embedding_model);
```

- [ ] **Step 2: Verify the SQL parses by provisioning a throwaway tenant**

Run:
```bash
set -a; source .env; set +a
go run ./cmd/ragctl enroll --slug drift-check --name "Drift Check" --embedding-dim 1024 --db-ssl-mode disable
```
Expected: `provisioned ... schema version 2` (no SQL error). Then clean up:
```bash
docker exec rag-platform-postgres-1 psql -U rag -d control_plane -tAc "select database_name, username from tenant_databases d join tenants t on t.id=d.tenant_id where t.slug='drift-check'"
# drop that database + role, then: delete from tenant_databases/tenants where slug='drift-check'
```

- [ ] **Step 3: Commit**

```bash
git add internal/migrate/tenant/00001_initial_schema.sql
git commit -m "schema: add chunks.content_hash for chunk-level drift reuse"
```

---

### Task 2: `embedcache` service

**Files:**
- Create: `internal/ingest/embedcache/embedcache.go`
- Test: `internal/ingest/embedcache/embedcache_test.go`

**Interfaces:**
- Produces:
  - `type Cache interface { Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error) }`
  - `func NewPgCache() Cache` — the production implementation.
  - Map keys are lowercase hex of each `content_hash`; use `hex.EncodeToString`.

- [ ] **Step 1: Write the failing test (row scanner, no live DB)**

The lookup SQL is exercised by e2e (Task 7); here unit-test the hex keying and miss handling with a fake `rows` behind a tiny seam. Create `embedcache_test.go`:
```go
package embedcache

import (
	"encoding/hex"
	"reflect"
	"testing"
)

func TestKeyByHexAndEmptyInput(t *testing.T) {
	// keyByHex maps parallel hashes+vectors to hex-keyed map.
	h1 := []byte{0xab, 0xcd}
	got := keyByHex([][]byte{h1}, [][]float32{{1, 2}})
	want := map[string][]float32{hex.EncodeToString(h1): {1, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyByHex = %v, want %v", got, want)
	}
	if len(keyByHex(nil, nil)) != 0 {
		t.Fatal("empty input must yield empty map")
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/ingest/embedcache/ -run TestKeyByHexAndEmptyInput -v`
Expected: FAIL (package/func not defined).

- [ ] **Step 3: Write the implementation**

Create `embedcache.go`:
```go
// Package embedcache reuses an existing chunk embedding for a chunk whose
// embed-text is byte-identical to one already embedded under the same model
// (chunk-level drift, SPEC-05 §1). It owns exactly the reuse lookup — the sink
// asks which chunk hashes are already known and skips re-embedding those.
package embedcache

import (
	"context"
	"encoding/hex"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Cache returns, for chunk-content hashes, the vector an existing chunk already
// holds for (hash, model). Hashes with no existing embedding are absent.
type Cache interface {
	Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error)
}

type pgCache struct{}

// NewPgCache returns the production Cache over a tenant database.
func NewPgCache() Cache { return pgCache{} }

func (pgCache) Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error) {
	if len(hashes) == 0 {
		return map[string][]float32{}, nil
	}
	rows, err := db.Query(ctx,
		`select distinct on (content_hash) content_hash, embedding
		 from chunks where content_hash = any($1) and embedding_model = $2`,
		hashes, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]float32, len(hashes))
	for rows.Next() {
		var h []byte
		var vec []float32
		if err := rows.Scan(&h, &vec); err != nil {
			return nil, err
		}
		out[hex.EncodeToString(h)] = vec
	}
	return out, rows.Err()
}

// keyByHex maps parallel hashes and vectors into a hex-keyed map (test helper +
// used by callers assembling a reuse map).
func keyByHex(hashes [][]byte, vecs [][]float32) map[string][]float32 {
	out := make(map[string][]float32, len(hashes))
	for i := range hashes {
		out[hex.EncodeToString(hashes[i])] = vecs[i]
	}
	return out
}
```
Note: pgx scans a `vector` column into `[]float32` via the pgvector registration already used by the store; if `Scan(&vec)` fails on the `vector` type, scan into `pgvector.Vector` (the type the store already imports) and call `.Slice()`. Match the store's existing pattern in `internal/documents/put.go`.

- [ ] **Step 4: Run it, verify it passes**

Run: `go test ./internal/ingest/embedcache/ -v`
Expected: PASS. Then `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/embedcache/
git commit -m "embedcache: reuse-lookup service for chunk-level drift"
```

---

### Task 3: Persist `content_hash` per chunk in the store

**Files:**
- Modify: `internal/documents/put.go` (`ChunkInput` struct + the `insert into chunks (...)` statement)
- Test: `internal/documents/put_test.go` (or the nearest existing put/store test)

**Interfaces:**
- Consumes: nothing new.
- Produces: `documents.ChunkInput` gains `ContentHash []byte`; the chunks insert writes it.

- [ ] **Step 1: Write the failing test**

Find the existing `ChunkInput` fields (near `type PutInput struct`, `Chunks []ChunkInput`). Add a test asserting a built `ChunkInput` round-trips `ContentHash` (a pure-struct test if there is no DB test, else extend the store e2e in Task 7). Minimal pure test in `put_test.go`:
```go
func TestChunkInputCarriesContentHash(t *testing.T) {
	c := ChunkInput{ContentHash: []byte{1, 2, 3}}
	if len(c.ContentHash) != 3 {
		t.Fatalf("ContentHash not carried")
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/documents/ -run TestChunkInputCarriesContentHash -v`
Expected: FAIL — `ContentHash` undefined.

- [ ] **Step 3: Implement**

In `internal/documents/put.go`, add to `ChunkInput`:
```go
	ContentHash []byte // sha256(embed-text); chunk-level drift key
```
In the `insert into chunks` statement (currently columns `(document_id, version_id, source_id, position, heading_path, content, token_count, embedding, embedding_model, metadata)`), add `content_hash` to the column list and a matching parameter, and pass `ch.ContentHash` in the `Exec`/batch args in the same position. Keep the existing `$8::vector` cast for `embedding`.

- [ ] **Step 4: Run it, verify it passes**

Run: `go test ./internal/documents/ -run TestChunkInputCarriesContentHash -v` then `go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/documents/put.go internal/documents/put_test.go
git commit -m "documents: persist content_hash per chunk"
```

---

### Task 4: Sink reuses embeddings for unchanged chunks

**Files:**
- Modify: `internal/ingest/sink/sink.go` (`Config`, `Stats`, `Sink.Put` steps 4–6, `putInput`)
- Test: `internal/ingest/sink/sink_test.go`

**Interfaces:**
- Consumes: `embedcache.Cache` (Task 2); `documents.ChunkInput.ContentHash` (Task 3).
- Produces: `sink.Config.Cache embedcache.Cache`; `Stats.ChunksEmbedded` / `Stats.ChunksReused`.

- [ ] **Step 1: Write the failing test**

Add to `sink_test.go` a fake cache and an embedder that counts calls. Assert: a changed document with 3 chunks where 2 hashes are known reuses 2 and embeds 1.
```go
type fakeCache struct{ known map[string][]float32 }

func (f fakeCache) Lookup(_ context.Context, _ *tenant.DB, _ string, hashes [][]byte) (map[string][]float32, error) {
	out := map[string][]float32{}
	for _, h := range hashes {
		if v, ok := f.known[hex.EncodeToString(h)]; ok {
			out[hex.EncodeToString(h)] = v
		}
	}
	return out, nil
}

func TestPutReusesKnownChunkEmbeddings(t *testing.T) {
	// Build a doc whose parse yields 3 chunks. Precompute the 3 embed-text hashes;
	// seed fakeCache with the first two. Use a fakeEmbedder that records how many
	// texts it was asked to embed and returns dim-4 vectors.
	// Assert: embedder saw exactly 1 text; committed PutInput has 3 chunks each with
	// a ContentHash; stats.ChunksReused == 2 and stats.ChunksEmbedded == 1.
	// (Mirror the fixture style of TestCompleteFullSyncSoftDeletesUnseen.)
	t.Skip("fill in with the package's existing doc/parse fixture — see sink_test.go helpers")
}
```
Replace the `t.Skip` body using the existing `fakeStore`/`fakeEmbedder`/document fixtures already in `sink_test.go` (a `Put` test already exists — copy its setup). The embedder fake must expose a `lastCount int`.

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/ingest/sink/ -run TestPutReusesKnownChunkEmbeddings -v`
Expected: FAIL (`Config.Cache` / stats fields undefined, or embedder called with 3).

- [ ] **Step 3: Implement**

In `sink.go`:
- Add to `Config`: `Cache embedcache.Cache` (import the package). If nil, treat every chunk as a miss (so existing tests/callers without a cache keep working).
- Add to `Stats`: `ChunksEmbedded int \`json:"chunks_embedded"\`` and `ChunksReused int \`json:"chunks_reused"\``.
- In `Sink.Put`, replace steps 4–5 (`chunks := chunk.Document(...)` … the single `Embed`) with:
```go
	chunks := chunk.Document(norm, s.cfg.Chunk)

	// Per-chunk content hash of the exact embed-text.
	hashes := make([][]byte, len(chunks))
	texts := embedTexts(chunks)
	for i, txt := range texts {
		sum := sha256.Sum256([]byte(txt))
		hashes[i] = sum[:]
	}

	// Reuse: ask the cache which hashes already have an embedding for this model.
	reuse := map[string][]float32{}
	if s.cfg.Cache != nil {
		reuse, err = s.cfg.Cache.Lookup(ctx, s.cfg.DB, s.cfg.Model, hashes)
		if err != nil {
			return err // infra error: retry
		}
	}

	// Embed only the misses (one batched call), preserving order.
	var missTexts []string
	var missIdx []int
	for i := range chunks {
		if _, hit := reuse[hex.EncodeToString(hashes[i])]; !hit {
			missTexts = append(missTexts, texts[i])
			missIdx = append(missIdx, i)
		}
	}
	vectors := make([][]float32, len(chunks))
	for i := range chunks { // fill hits first
		if v, hit := reuse[hex.EncodeToString(hashes[i])]; hit {
			vectors[i] = v
		}
	}
	if len(missTexts) > 0 {
		res, embErr := s.cfg.Embedder.Embed(ctx, missTexts)
		if embErr != nil {
			if errors.Is(embErr, embed.ErrCircuitOpen) {
				return &SnoozeError{Err: embErr}
			}
			s.recordFailure(doc.ExternalID, embErr)
			return nil
		}
		if len(res.Vectors) != len(missTexts) {
			return fmt.Errorf("sink: embedder returned %d vectors for %d chunks", len(res.Vectors), len(missTexts))
		}
		for j, idx := range missIdx {
			vectors[idx] = res.Vectors[j]
		}
		s.cfg.Metrics.AddEmbedTokens(s.cfg.Tenant, s.cfg.Provider, res.Tokens)
	}
	s.stats.ChunksEmbedded += len(missTexts)
	s.stats.ChunksReused += len(chunks) - len(missTexts)
```
- Change `putInput(...)` to accept `hashes [][]byte` and set each `ChunkInput.ContentHash = hashes[i]`. Keep passing `vectors`.
- Remove the now-duplicated `AddEmbedTokens` call that followed the old single `Embed` (tokens are added inside the miss branch above; do not double-count).
- Ensure imports: `crypto/sha256`, `encoding/hex`, `github.com/rag-platform/ragctl/internal/ingest/embedcache`.

- [ ] **Step 4: Run it, verify it passes**

Run: `go test ./internal/ingest/sink/ -v` then `go build ./...`
Expected: PASS (including existing sink tests — nil Cache path keeps them green).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/ingest/sink/sink.go
git add internal/ingest/sink/
git commit -m "sink: reuse embeddings for unchanged chunks (chunk-level drift)"
```

---

### Task 5: Wire the cache at the composition root

**Files:**
- Modify: `internal/cli/api_server.go` (the `sink.Config` / `documents.NewService` build, and the worker sink build if separate)
- Modify: `internal/worker/*.go` if the worker builds its own `sink.Config` (search for `sink.New(sink.Config{`)

**Interfaces:**
- Consumes: `embedcache.NewPgCache()` (Task 2), `sink.Config.Cache` (Task 4).

- [ ] **Step 1: Find every `sink.Config{` / `sink.New(` construction**

Run: `grep -rn "sink.Config{" internal/ --include=*.go | grep -v _test`
Note each site (documents service build in `api_server.go`; the sync/ingest workers via `internal/worker`).

- [ ] **Step 2: Pass the cache at each site**

At each `sink.Config{...}` literal, add `Cache: embedcache.NewPgCache(),` (import `internal/ingest/embedcache`). Construct one `embedcache.NewPgCache()` and reuse it if the site builds many sinks.

- [ ] **Step 3: Build**

Run: `go build ./... && go test ./internal/cli/ ./internal/worker/ 2>&1 | tail`
Expected: build clean, existing tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/api_server.go internal/worker/
git commit -m "wire embedcache into the ingest sink"
```

---

### Task 6: Metric for reuse rate

**Files:**
- Modify: `internal/obs/metrics.go` (add counter + accessor) — match the existing `AddIngestChunks` pattern
- Modify: `internal/ingest/sink/sink.go` (increment on reuse)
- Test: `internal/ingest/sink/metrics_test.go` or the obs test

**Interfaces:**
- Produces: `Metrics.AddEmbedChunksReused(tenant, provider string, n int)`.

- [ ] **Step 1: Write the failing test**

In the sink metrics test, assert a reuse increments `embed_chunks_reused_total{...}` (mirror the existing `AddIngestChunks`/`AddEmbedTokens` metric test).

- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/obs/ ./internal/ingest/sink/ -run Reused -v` → FAIL.

- [ ] **Step 3: Implement**

Add a `CounterVec` `embed_chunks_reused_total` (labels `tenant`, `provider`) in `internal/obs/metrics.go` with `func (m *Metrics) AddEmbedChunksReused(tenant, provider string, n int)` guarding `m == nil`. In `sink.Put`, after computing reuse count, call `s.cfg.Metrics.AddEmbedChunksReused(s.cfg.Tenant, s.cfg.Provider, len(chunks)-len(missTexts))`.

- [ ] **Step 4: Run it, verify it passes** — `go test ./internal/obs/ ./internal/ingest/sink/ -v` then `go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/obs/ internal/ingest/sink/
git commit -m "metrics: embed_chunks_reused_total"
```

---

### Task 7: e2e — re-ingest reuses embeddings

**Files:**
- Create/Modify: `test/e2e/embedcache_e2e_test.go` (build tag `e2e`)

**Interfaces:**
- Consumes: the full stack (live Postgres + vchord). Follows the fixture style of `test/e2e/document_store_e2e_test.go`.

- [ ] **Step 1: Write the test**

Provision a tenant, run the sink over a document (N chunks) once (all embedded), then over a version with **one chunk's text changed** (N-1 identical hashes). Use a real `embedcache.NewPgCache()` and an embedder fake that records call counts. Assert the second run embeds exactly 1 chunk and reuses N-1, and that the committed chunks carry the right `content_hash`.

- [ ] **Step 2: Run it (needs the live stack)**

Run: `set -a; source .env; set +a; go test -tags e2e ./test/e2e/ -run EmbedCache -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/embedcache_e2e_test.go
git commit -m "e2e: chunk-level drift reuses embeddings on re-ingest"
```

---

### Task 8: ADR + spec status

**Files:**
- Create: `docs/adr/0077-chunk-level-drift-detection.md` (from `docs/adr/TEMPLATE.md`)
- Modify: the design spec status → Implemented

- [ ] **Step 1:** Write ADR-0077 recording the decision (content-hash chunk reuse, tenant-wide, Approach A over a refcounted store), tracing SPEC-05 §1/§5 and ADR-0008.
- [ ] **Step 2:** Set the spec header `Status:` to `Implemented`.
- [ ] **Step 3: Commit**

```bash
git add docs/adr/0077-chunk-level-drift-detection.md docs/superpowers/specs/2026-09-17-chunk-level-drift-detection-design.md
git commit -m "ADR-0077: chunk-level drift detection"
```

---

## Self-Review

- **Spec coverage:** service (T2), schema (T1), store persistence (T3), sink reuse + stats (T4), wiring (T5), metric (T6), e2e (T7), ADR (T8) — every spec section maps to a task.
- **Type consistency:** `Cache.Lookup(ctx, *tenant.DB, model string, [][]byte) (map[string][]float32, error)` used identically in T2/T4/T5; `ChunkInput.ContentHash []byte` in T3/T4; `Stats.ChunksEmbedded/ChunksReused` in T4/T6.
- **Open confirmation for the executor:** T2 notes the pgvector scan type — match `internal/documents/put.go`'s existing `vector` read pattern rather than guessing; T4's test body must be filled from the existing `sink_test.go` `Put` fixture (the plan flags this explicitly rather than inventing a fixture).
