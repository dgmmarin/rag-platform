package tenants

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminPoolStore implements AdminStore over the control-plane pgx pool. It touches
// only control-plane tables (tenants, jobs) — never a tenant database (ADR-0003,
// C-3). The registry is the platform-admin scope's data, so unlike the /v1
// surface these reads are deliberately NOT filtered by a single tenant.
type AdminPoolStore struct{ pool *pgxpool.Pool }

// AdminStoreFromPool wraps a control-plane pool as an AdminStore.
func AdminStoreFromPool(pool *pgxpool.Pool) AdminPoolStore { return AdminPoolStore{pool: pool} }

// tenantColumns is the shared projection scanned into a Tenant. It never selects
// connection details or secrets (C-4).
const tenantColumns = `id::text, slug, name, status::text, region, created_at, updated_at, deleted_at, delete_after`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	if err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.Region,
		&t.CreatedAt, &t.UpdatedAt, &t.DeletedAt, &t.DeleteAfter); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// List returns tenants newest-first (by created_at), using keyset pagination on
// (created_at, id). A nil cursor starts from the newest row.
func (s AdminPoolStore) List(ctx context.Context, limit int, cur *tenantCursor) ([]Tenant, error) {
	args := []any{}
	where := ""
	if cur != nil {
		args = append(args, cur.CreatedAt, cur.ID)
		where = `where (created_at < $1 or (created_at = $1 and id < $2::uuid)) `
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx,
		`select `+tenantColumns+` from tenants `+where+
			`order by created_at desc, id desc limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetByID returns one tenant by id, or ErrTenantNotFound.
func (s AdminPoolStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	if !validUUID(id) {
		return Tenant{}, ErrTenantNotFound
	}
	t, err := scanTenant(s.pool.QueryRow(ctx,
		`select `+tenantColumns+` from tenants where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrTenantNotFound
	}
	return t, err
}

// RecordProvisionJob writes the provision_tenant mirror row for a just-provisioned
// tenant and returns its id. Provisioning ran synchronously and completed before
// this is called (STORY-02.3 precedent, ADR-0016), so the row is a truthful
// `succeeded` record in the history/mirror view (ADR-0005) — not a placeholder
// that no worker will ever run. When EPIC-09 wires River, POST /admin/tenants
// becomes a real async enqueue (a `queued` row consumed by a provision_tenant
// worker) and this synchronous record goes away.
func (s AdminPoolStore) RecordProvisionJob(ctx context.Context, tenantID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`insert into jobs (tenant_id, kind, status, started_at, finished_at)
		 values ($1, 'provision_tenant', 'succeeded', now(), now())
		 returning id::text`, tenantID).Scan(&id)
	return id, err
}

// validUUID is a cheap guard so a non-UUID id scans as ErrTenantNotFound rather
// than raising a Postgres type error.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
