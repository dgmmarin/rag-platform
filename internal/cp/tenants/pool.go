package tenants

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolDB adapts *pgxpool.Pool to SettingsDB. pgx.Row satisfies rowScanner and
// pgconn.CommandTag satisfies commandTag, so the adapter is a thin pass-through.
type poolDB struct{ pool *pgxpool.Pool }

// SettingsFromPool wraps a pgx pool as the settings service's DB.
func SettingsFromPool(pool *pgxpool.Pool) SettingsDB { return poolDB{pool: pool} }

func (p poolDB) QueryRow(ctx context.Context, sql string, args ...any) rowScanner {
	return p.pool.QueryRow(ctx, sql, args...)
}

func (p poolDB) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return tag, err
}
