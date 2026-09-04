// Package retrieve runs the SPEC-06 §2 hybrid retrieval query: a single SQL
// round trip that ranks the tenant's live_chunks by vector similarity and by
// full-text relevance, fuses the two rankings with reciprocal rank fusion
// (RRF, k=60) and returns the top-k fused chunks (FR-RET-01/02/08, ADR-0007).
//
// It consumes a query embedding (produced by the caller — the query API in
// STORY-08.2 embeds the incoming question via internal/ingest/embed) plus the
// raw query text for the full-text side, so this package is embedding-provider
// agnostic. Reranking (STORY-08.3), the min_score grounding floor and the LLM
// answer path (STORY-08.5) live downstream; the Result slice this package
// returns is the seam they consume.
//
// All tenant data is reached ONLY through a *tenant.DB handle from the resolver
// (ADR-0003, C-1, C-3); this package never touches a raw pool. hnsw.ef_search is
// tuned per query with a transaction-local set_config so the GUC never leaks back
// to the pooled connection.
package retrieve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// rrfK is the reciprocal-rank-fusion constant from ADR-0007 / SPEC-06 §2. It is
// duplicated in the SQL literal (sum(1.0/(60+r))); RRFScore states it in Go so a
// test can prove the two agree.
const rrfK = 60

// Retrieval defaults from ADR-0007: top-K (default 40) from each retriever, fused
// down to the final top-k (default 8) passed to generation.
const (
	defaultKVector = 40
	defaultKText   = 40
	defaultK       = 8
	minEFSearch    = 40
)

// ErrNoEmbedding is returned when Retrieve is called without a query embedding;
// the vector side of the hybrid query cannot run without one.
var ErrNoEmbedding = errors.New("retrieve: query embedding is required")

// Filters narrows retrieval before ranking (FR-RET-02). Every field is optional;
// an unset field is a no-op (its `$n is null or ...` guard passes every row). The
// filters are applied identically to both the vector and full-text CTEs so the
// two rankings see the same candidate set (SPEC-06 §2).
type Filters struct {
	// SourceIDs restricts to chunks whose source_id is in this set (control-plane
	// source ids, denormalised onto chunks). Empty means no source filter.
	SourceIDs []string
	// URIPrefix restricts to documents whose canonical uri starts with this
	// prefix (e.g. "https://docs.acme.com/"). LIKE metacharacters in it are
	// escaped so a client cannot inject a wildcard.
	URIPrefix string
	// DateFrom / DateTo bound the chunk creation time (live_chunks.created_at,
	// the indexing timestamp — the only timestamp the view exposes). Nil means
	// unbounded on that side.
	DateFrom *time.Time
	DateTo   *time.Time
	// MetadataTags is a JSON object matched by containment against the document
	// metadata (documents.metadata @> tags), backed by its gin jsonb_path_ops
	// index. Empty means no metadata filter.
	MetadataTags json.RawMessage
}

// Params is one hybrid retrieval request.
type Params struct {
	// Embedding is the query vector ($1). Required; its dimension must match the
	// tenant's configured embedding_dim (enforced by the vector(N) column).
	Embedding []float32
	// QueryText is the raw question ($4) fed to websearch_to_tsquery for the
	// full-text side. May be empty (vector-only retrieval).
	QueryText string
	// KVector / KText are the per-retriever candidate limits ($3 / $5). K is the
	// final fused result count ($6). Zero values take the ADR-0007 defaults.
	KVector int
	KText   int
	K       int
	// Filters narrows both CTEs before ranking.
	Filters Filters
}

// Result is one ranked chunk with its fused RRF score and the metadata a citation
// (STORY-08.5) needs. The embedding vector itself is never returned.
type Result struct {
	ChunkID     string
	DocumentID  string
	SourceID    string
	Content     string
	URI         string
	Title       string
	HeadingPath []string
	Metadata    json.RawMessage
	Score       float64
}

func (p Params) withDefaults() Params {
	if p.KVector <= 0 {
		p.KVector = defaultKVector
	}
	if p.KText <= 0 {
		p.KText = defaultKText
	}
	if p.K <= 0 {
		p.K = defaultK
	}
	return p
}

// Retrieve runs the SPEC-06 §2 hybrid query against the tenant database and
// returns the top-k fused chunks, highest score first. It considers only live
// chunks — the current version of active documents, the live_chunks semantics,
// enforced here by the version_id semi-join (see hybridSQL / ADR-0051) — so
// soft-deleted or superseded content is never returned.
//
// hnsw.ef_search and hnsw.iterative_scan are tuned for this query via
// transaction-local set_config inside a read-only transaction, so the tuning
// applies to this query alone and never leaks to the next user of the pooled
// connection (see withEFSearchTx). A read-only transaction is used so retrieval
// also works on a suspended (read-only) tenant.
func Retrieve(ctx context.Context, db *tenant.DB, p Params) ([]Result, error) {
	if len(p.Embedding) == 0 {
		return nil, ErrNoEmbedding
	}
	p = p.withDefaults()

	var out []Result
	err := withEFSearchTx(ctx, db, p.KVector, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, hybridSQL, buildArgs(p)...)
		if err != nil {
			return fmt.Errorf("retrieve: hybrid query: %w", err)
		}
		defer rows.Close()
		out, err = scanResults(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readBeginner opens a read-only transaction; *tenant.DB satisfies it via
// BeginRead. It is the seam that lets withEFSearchTx be exercised with a fake tx.
type readBeginner interface {
	BeginRead(ctx context.Context) (pgx.Tx, error)
}

// rowScanner is the read subset of pgx.Rows scanResults needs; pgx.Rows satisfies
// it, and a fake satisfies it in tests, so the row→Result mapping is unit-testable
// without a database.
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// scanResults maps the hybrid query's result rows onto []Result, in the column
// order hybridSQL projects. It is the row-mapping half of Retrieve, kept pure so it
// is exercised by unit tests; the SQL execution around it is covered by the e2e
// suite (real Postgres+pgvector).
func scanResults(rows rowScanner) ([]Result, error) {
	var out []Result
	for rows.Next() {
		var r Result
		var meta []byte
		if err := rows.Scan(&r.ChunkID, &r.DocumentID, &r.SourceID, &r.Content,
			&r.URI, &r.Title, &r.HeadingPath, &meta, &r.Score); err != nil {
			return nil, fmt.Errorf("retrieve: scan: %w", err)
		}
		if len(meta) > 0 {
			r.Metadata = json.RawMessage(meta)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("retrieve: rows: %w", err)
	}
	return out, nil
}

// Explain returns the PostgreSQL execution plan the planner naturally chooses for
// the hybrid query with the given params (EXPLAIN ANALYZE, text format), run under
// the SAME ef_search transaction Retrieve uses. It exists for benchmarking and
// operability — e.g. to confirm the plan uses the hnsw and gin indexes at scale
// (STORY-08.1 AC, SPEC-06 §7). It never returns rows to a client.
func Explain(ctx context.Context, db *tenant.DB, p Params) (string, error) {
	return explain(ctx, db, p, false)
}

// ExplainIndexed returns the plan with sequential scans disabled, forcing the
// planner onto the available indexes if the query is index-eligible. It is a
// benchmark/diagnostic aid: on a small local corpus a seq scan is cheaper, so the
// natural plan (Explain) may not touch the hnsw/gin indexes; this variant confirms
// the hybrid query CAN use them — at production scale the planner adopts them
// without the nudge.
func ExplainIndexed(ctx context.Context, db *tenant.DB, p Params) (string, error) {
	return explain(ctx, db, p, true)
}

func explain(ctx context.Context, db *tenant.DB, p Params, forceIndex bool) (string, error) {
	if len(p.Embedding) == 0 {
		return "", ErrNoEmbedding
	}
	p = p.withDefaults()

	var plan string
	err := withEFSearchTx(ctx, db, p.KVector, func(ctx context.Context, tx pgx.Tx) error {
		if forceIndex {
			if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
				return fmt.Errorf("retrieve: disable seqscan: %w", err)
			}
		}
		rows, err := tx.Query(ctx, "explain (analyze, buffers, format text) "+hybridSQL, buildArgs(p)...)
		if err != nil {
			return fmt.Errorf("retrieve: explain: %w", err)
		}
		defer rows.Close()
		plan, err = readPlan(rows)
		return err
	})
	return plan, err
}

// readPlan concatenates the one-column-per-line rows of an EXPLAIN into a single
// string. Pure (row-driven), so it is unit-testable via a fake rowScanner.
func readPlan(rows rowScanner) (string, error) {
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

// withEFSearchTx opens a read-only transaction (permitted on suspended tenants),
// tunes the query GUCs transaction-locally, and runs fn. Three GUCs are set
// (SPEC-06 §2, ADR-0051):
//   - hnsw.ef_search = max(40, kVector): a wider search list for larger candidate
//     requests keeps recall high.
//   - hnsw.iterative_scan = strict_order: lets the hnsw index drive the vector CTE
//     even though it carries the liveVersions/source/date filters, resuming the
//     index scan until it has $3 filtered rows IN EXACT distance order (so the RRF
//     ranks are the true nearest-neighbour ranks). Without it a filtered
//     `order by <=> limit` falls back to a full sort and never uses the index.
//   - plan_cache_mode = force_custom_plan: the optional filters are
//     `$n is null or <predicate>` guards; a GENERIC prepared-statement plan (which
//     the pgx statement cache switches to after a few executions) cannot see a
//     parameter is NULL, so it cannot prune the guard and picks a full-scan plan
//     over the hnsw index — measured ~180 ms vs ~15 ms for the custom plan at 100 k
//     chunks (a 12× regression, ADR-0051). Forcing a custom plan re-plans per
//     execution with the actual NULLs, restoring index selection. (This is why the
//     bench's per-call latency dwarfed a one-off EXPLAIN, which is always custom.)
//
// GUCs cannot be bound parameters; set_config's value arg can, and is_local=true
// scopes them to this transaction so nothing leaks to the pooled connection. The
// ef_search value is a validated int (>= 40), so no user string reaches the GUC.
// iterative_scan requires pgvector >= 0.8 (the platform's pinned image); on an
// older extension this SET errors, surfacing the misconfiguration loudly rather
// than silently serving slow full-sort retrieval. The read-only tx is always
// rolled back — there is nothing to commit.
//
// It takes a readBeginner (satisfied by *tenant.DB) rather than the concrete
// handle, so the GUC-tuning/tx lifecycle is unit-testable with a fake tx.
func withEFSearchTx(ctx context.Context, db readBeginner, kVector int, fn func(context.Context, pgx.Tx) error) error {
	tx, err := db.BeginRead(ctx)
	if err != nil {
		return fmt.Errorf("retrieve: begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// All three GUCs in one round trip. iterative_scan requires pgvector >= 0.8, so a
	// failure here most likely means an older extension (surfaced loudly).
	if _, err := tx.Exec(ctx, `select set_config('hnsw.ef_search', $1, true),
		set_config('hnsw.iterative_scan', 'strict_order', true),
		set_config('plan_cache_mode', 'force_custom_plan', true)`,
		strconv.Itoa(efSearch(kVector))); err != nil {
		return fmt.Errorf("retrieve: tune query GUCs (iterative_scan requires pgvector >= 0.8): %w", err)
	}
	return fn(ctx, tx)
}

// RRFScore is the reciprocal-rank-fusion score for a chunk given its 1-based rank
// in each list it appears in: sum(1/(60+r)). It is the Go statement of the SQL's
// sum(1.0/(60+r)) fusion (SPEC-06 §2), used as the test oracle so the SQL and the
// specification cannot silently diverge.
func RRFScore(ranks ...int) float64 {
	var s float64
	for _, r := range ranks {
		s += 1.0 / (float64(rrfK) + float64(r))
	}
	return s
}

// efSearch is the per-query hnsw.ef_search value: max(40, KVector) (SPEC-06 §2).
// A larger candidate request needs a wider search list to keep recall high.
func efSearch(kVector int) int {
	if kVector > minEFSearch {
		return kVector
	}
	return minEFSearch
}

// likePrefixPattern turns a user-supplied URI prefix into a safe anchored LIKE
// pattern: it escapes the LIKE metacharacters (%, _ and the escape char \) so the
// client cannot inject a wildcard, then appends a trailing % for the prefix
// match. The SQL binds it with `like $n escape '\'`.
func likePrefixPattern(prefix string) string {
	var b bytes.Buffer
	for _, r := range prefix {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

// buildArgs lays out the $1..$10 binds for hybridSQL. An absent filter becomes a
// nil (SQL NULL) bind so its `$n is null or ...` guard is a no-op, rather than an
// empty array/string that would exclude every row.
func buildArgs(p Params) []any {
	var sources any
	if len(p.Filters.SourceIDs) > 0 {
		sources = p.Filters.SourceIDs
	}
	var uri any
	if p.Filters.URIPrefix != "" {
		uri = likePrefixPattern(p.Filters.URIPrefix)
	}
	var from any
	if p.Filters.DateFrom != nil {
		from = *p.Filters.DateFrom
	}
	var to any
	if p.Filters.DateTo != nil {
		to = *p.Filters.DateTo
	}
	var tags any
	if len(p.Filters.MetadataTags) > 0 {
		tags = string(p.Filters.MetadataTags)
	}
	return []any{
		vectorLiteral(p.Embedding), // $1
		sources,                    // $2 ::uuid[]
		p.KVector,                  // $3
		p.QueryText,                // $4
		p.KText,                    // $5
		p.K,                        // $6
		uri,                        // $7 ::text (LIKE pattern)
		from,                       // $8 ::timestamptz
		to,                         // $9 ::timestamptz
		tags,                       // $10 ::jsonb
	}
}

// vectorLiteral renders a []float32 as a pgvector text literal ("[a,b,c]") so the
// query embedding can be bound with $1::vector, avoiding a pgvector codec
// dependency for one parameter (matches internal/documents.vectorLiteral).
func vectorLiteral(v []float32) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// liveVersions is the liveness predicate shared by both CTEs. A chunk is live iff
// its version_id is the current_version of an active document — exactly the
// live_chunks view definition (SPEC-03 §2), but expressed as a semi-join on
// version_id rather than a join to documents. version_id is globally unique, so a
// chunk's version_id is in this set iff it is its own document's current version;
// the two forms are semantically identical. The semi-join form is required for the
// vector CTE: ordering by `embedding <=> $1` over the live_chunks VIEW (whose join
// the planner must resolve before it knows which chunks are live) forces a full
// sort and never uses the hnsw index, so a `from live_chunks order by <=> limit`
// query costs O(all live chunks) — ~180 ms at 50 k, seconds at 1 M. Filtering
// chunks directly with `version_id in (…)` lets pgvector's iterative index scan
// (ADR-0051) drive the hnsw index and post-filter, restoring index-time retrieval.
// The optional document-level filters (uri prefix, metadata tags) fold into this
// subquery so they narrow the candidate set without a second documents join.
const liveVersions = `c.version_id in (
        select d.current_version from documents d
        where d.status = 'active'
          and ($7::text is null or d.uri like $7 escape '\')
          and ($10::jsonb is null or d.metadata @> $10))`

// hybridSQL is the SPEC-06 §2 hybrid retrieval query: one round trip fusing a
// vector-ranked CTE (v) and a full-text-ranked CTE (t) with RRF (k=60) in CTE f,
// joined back to chunks + documents for the returned metadata. Liveness and the
// uri/metadata document filters are the liveVersions semi-join; the source-id and
// date-range filters are chunk-column `$n is null or ...` guards, so an unset
// filter is a no-op. Both CTEs use the identical filter set so the two rankings
// see the same candidate set. The hnsw and gin indexes both back this query when
// hnsw.iterative_scan is set (ADR-0051 / SPEC-06 §2).
var hybridSQL = `
with v as (
    select c.id,
           row_number() over (order by c.embedding <=> $1::vector) as r
    from chunks c
    where ` + liveVersions + `
      and ($2::uuid[] is null or c.source_id = any($2))
      and ($8::timestamptz is null or c.created_at >= $8)
      and ($9::timestamptz is null or c.created_at <= $9)
    order by c.embedding <=> $1::vector
    limit $3
),
t as (
    select c.id,
           row_number() over (order by ts_rank_cd(c.tsv, q) desc) as r
    from chunks c, websearch_to_tsquery('simple', $4) q
    where c.tsv @@ q
      and ` + liveVersions + `
      and ($2::uuid[] is null or c.source_id = any($2))
      and ($8::timestamptz is null or c.created_at >= $8)
      and ($9::timestamptz is null or c.created_at <= $9)
    order by ts_rank_cd(c.tsv, q) desc
    limit $5
),
f as (
    select id, sum(1.0 / (60 + r))::float8 as score
    from (select * from v union all select * from t) u
    group by id
)
select c.id::text, c.document_id::text, c.source_id::text,
       c.content, coalesce(d.uri, ''), coalesce(d.title, ''),
       c.heading_path, c.metadata, f.score
from f
join chunks c on c.id = f.id
join documents d on d.id = c.document_id
order by f.score desc
limit $6;`
