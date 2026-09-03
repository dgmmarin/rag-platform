package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolDB implements Store over the control-plane pgx pool. It touches only the
// control-plane `jobs` table (C-3: the jobs table is the history/mirror view,
// never tenant content) — it never opens a tenant database (ADR-0003). Every
// query is filtered by tenant_id so the store can never cross the tenant boundary
// (FR-ACC-03).
type PoolDB struct{ pool *pgxpool.Pool }

// FromPool wraps a control-plane pool as a jobs Store.
func FromPool(pool *pgxpool.Pool) PoolDB { return PoolDB{pool: pool} }

// jobColumns is the shared projection scanned into a Job.
const jobColumns = `id::text, tenant_id::text, source_id::text, kind::text, status::text,
	attempt, max_attempts, stats, error, queued_at, started_at, finished_at, worker_id`

// scanJob scans one row in jobColumns order.
func scanJob(row pgx.Row) (Job, error) {
	var j Job
	var stats []byte
	err := row.Scan(&j.ID, &j.TenantID, &j.SourceID, &j.Kind, &j.Status,
		&j.Attempt, &j.MaxAttempts, &stats, &j.Error, &j.QueuedAt,
		&j.StartedAt, &j.FinishedAt, &j.WorkerID)
	if err != nil {
		return Job{}, err
	}
	if len(stats) > 0 {
		j.Stats = json.RawMessage(stats)
	}
	return j, nil
}

// List returns a tenant's jobs newest-first, using keyset pagination on
// (queued_at, id) and applying the optional status/kind/source filters. A nil
// cursor starts from the newest row.
func (p PoolDB) List(ctx context.Context, tenantID string, flt ListFilter, limit int, cur *Cursor) ([]Job, error) {
	// tenant_id ($1) + cursor ($2 queued_at, $3 id) are always present; filters
	// are appended positionally so an empty filter adds no predicate.
	args := []any{tenantID}
	var curAt, curID any
	if cur != nil {
		curAt = cur.QueuedAt
		curID = cur.ID
	}
	args = append(args, curAt, curID)

	where := `where tenant_id = $1
		and ($2::timestamptz is null
		     or queued_at < $2::timestamptz
		     or (queued_at = $2::timestamptz and id < $3::uuid))`
	if flt.Status != "" {
		args = append(args, flt.Status)
		where += " and status = $" + strconv.Itoa(len(args)) + "::job_status"
	}
	if flt.Kind != "" {
		args = append(args, flt.Kind)
		where += " and kind = $" + strconv.Itoa(len(args)) + "::job_kind"
	}
	if flt.SourceID != "" {
		if !validUUID(flt.SourceID) {
			// A non-UUID source filter matches nothing rather than raising a
			// Postgres type error.
			return []Job{}, nil
		}
		args = append(args, flt.SourceID)
		where += " and source_id = $" + strconv.Itoa(len(args)) + "::uuid"
	}
	args = append(args, limit)

	rows, err := p.pool.Query(ctx,
		`select `+jobColumns+` from jobs `+where+
			` order by queued_at desc, id desc limit $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Get returns one tenant-scoped job or ErrNotFound.
func (p PoolDB) Get(ctx context.Context, tenantID, id string) (Job, error) {
	if !validUUID(id) {
		return Job{}, ErrNotFound
	}
	j, err := scanJob(p.pool.QueryRow(ctx,
		`select `+jobColumns+` from jobs where tenant_id = $1 and id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

// CancelQueued flips a queued job to cancelled in one guarded statement, so the
// transition is race-safe: only a row still in 'queued' is affected. It stamps
// finished_at (a cancelled queued job never ran, so started_at stays null). When
// the row is not queued (or gone) no row is returned and changed is false.
func (p PoolDB) CancelQueued(ctx context.Context, tenantID, id string) (Job, bool, error) {
	if !validUUID(id) {
		return Job{}, false, nil
	}
	j, err := scanJob(p.pool.QueryRow(ctx, `
		update jobs set status = 'cancelled', finished_at = now()
		where tenant_id = $1 and id = $2 and status = 'queued'
		returning `+jobColumns, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// validUUID is a cheap guard so a non-UUID path segment scans as ErrNotFound
// rather than raising a Postgres type error.
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
