package worker

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/obs"
)

// SampleTenantSchemaMismatch counts the active tenants whose recorded schema_version
// is behind expected (the binary's highest embedded tenant migration version,
// migrate.ExpectedTenantVersion) and records it on tenant_schema_mismatch (SPEC-10
// §2/§5: the migration-mismatch alert). It reads the control-plane registry only
// (tenants + tenant_databases, C-3) — no tenant DB is opened. A tenant with no
// tenant_databases row (still provisioning) has no recorded version yet and is not
// counted as behind.
func SampleTenantSchemaMismatch(ctx context.Context, pool *pgxpool.Pool, expected int64, m *obs.Metrics) error {
	var n int
	err := pool.QueryRow(ctx, `
		select count(*)
		  from tenants t
		  join tenant_databases d on d.tenant_id = t.id
		 where t.status = 'active' and coalesce(d.schema_version, 0) < $1`, expected).Scan(&n)
	if err != nil {
		return err
	}
	m.SetTenantSchemaMismatch(n)
	return nil
}
