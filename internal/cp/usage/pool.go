package usage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// upsertSQL is the accumulating upsert the flusher runs per (tenant, day)
// (SPEC-10 §6). Every counter column is summed onto the existing row rather than
// overwritten, so repeated flushes — and concurrent replicas — add correctly.
const upsertSQL = `insert into usage_daily
    (tenant_id, day, queries, docs_ingested, chunks_embedded, embed_tokens, llm_in_tokens, llm_out_tokens)
 values ($1, $2, $3, $4, $5, $6, $7, $8)
 on conflict (tenant_id, day) do update set
    queries = usage_daily.queries + excluded.queries,
    docs_ingested = usage_daily.docs_ingested + excluded.docs_ingested,
    chunks_embedded = usage_daily.chunks_embedded + excluded.chunks_embedded,
    embed_tokens = usage_daily.embed_tokens + excluded.embed_tokens,
    llm_in_tokens = usage_daily.llm_in_tokens + excluded.llm_in_tokens,
    llm_out_tokens = usage_daily.llm_out_tokens + excluded.llm_out_tokens`

// PoolDB adapts *pgxpool.Pool to both UpsertDB (Counter flush) and QueryDB
// (Service read). pgx.Rows satisfies Rows, so the read side is a thin pass-through.
// It is exported so FromPool can return a named type usable by both NewCounter and
// NewService.
type PoolDB struct{ pool *pgxpool.Pool }

// FromPool wraps a pgx pool for both the usage flusher and reader.
func FromPool(pool *pgxpool.Pool) PoolDB { return PoolDB{pool: pool} }

// UpsertUsage runs the accumulating usage_daily upsert for one (tenant, day).
func (p PoolDB) UpsertUsage(ctx context.Context, tenantID string, day time.Time, d Delta) error {
	_, err := p.pool.Exec(ctx, upsertSQL,
		tenantID, day, d.Queries, d.DocsIngested, d.ChunksEmbedded,
		d.EmbedTokens, d.LLMInTokens, d.LLMOutTokens)
	return err
}

// Query runs a read query and adapts pgx.Rows to the reader's Rows interface.
func (p PoolDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
