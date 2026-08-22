package audit

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolDB adapts *pgxpool.Pool to both ExecDB (Record) and QueryDB (Service).
// pgconn.CommandTag satisfies commandTag and pgx.Rows satisfies Rows, so the
// adapter is a thin pass-through.
type poolDB struct{ pool *pgxpool.Pool }

// FromPool wraps a pgx pool for both the audit writer and reader.
func FromPool(pool *pgxpool.Pool) poolDB { return poolDB{pool: pool} }

func (p poolDB) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return tag, err
}

func (p poolDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}
