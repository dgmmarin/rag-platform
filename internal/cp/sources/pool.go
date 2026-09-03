package sources

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolDB implements Store over the control-plane pgx pool. It touches only
// control-plane tables (sources, jobs) — never a tenant database (C-3). Every
// query is filtered by tenant_id so the store can never cross the tenant boundary.
type PoolDB struct{ pool *pgxpool.Pool }

// FromPool wraps a control-plane pool as a sources Store.
func FromPool(pool *pgxpool.Pool) PoolDB { return PoolDB{pool: pool} }

// sourceColumns is the shared projection scanned into a Source. credentials_enc is
// deliberately never selected (FR-SRC-10).
const sourceColumns = `id::text, tenant_id::text, kind::text, name, status::text, config,
	schedule_cron, next_run_at, last_run_at, last_success_at, last_error, created_at, updated_at`

// scanSource scans one row in sourceColumns order.
func scanSource(row pgx.Row) (Source, error) {
	var s Source
	var cfg []byte
	err := row.Scan(&s.ID, &s.TenantID, &s.Kind, &s.Name, &s.Status, &cfg,
		&s.ScheduleCron, &s.NextRunAt, &s.LastRunAt, &s.LastSuccessAt, &s.LastError,
		&s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return Source{}, err
	}
	s.Config = json.RawMessage(cfg)
	return s, nil
}

// List returns a tenant's sources newest-first, using keyset pagination on
// (created_at, id). A nil cursor starts from the newest row.
func (p PoolDB) List(ctx context.Context, tenantID string, limit int, cur *Cursor) ([]Source, error) {
	var (
		curAt any
		curID any
	)
	if cur != nil {
		curAt = cur.CreatedAt
		curID = cur.ID
	}
	rows, err := p.pool.Query(ctx, `
		select `+sourceColumns+`
		from sources
		where tenant_id = $1
		  and ($2::timestamptz is null
		       or created_at < $2::timestamptz
		       or (created_at = $2::timestamptz and id < $3::uuid))
		order by created_at desc, id desc
		limit $4`,
		tenantID, curAt, curID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns one tenant-scoped source or ErrNotFound.
func (p PoolDB) Get(ctx context.Context, tenantID, id string) (Source, error) {
	if !validUUID(id) {
		return Source{}, ErrNotFound
	}
	s, err := scanSource(p.pool.QueryRow(ctx,
		`select `+sourceColumns+` from sources where tenant_id = $1 and id = $2`, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	return s, err
}

// Create inserts a new source, mapping the (tenant_id, name) unique violation to
// ErrDuplicateName.
func (p PoolDB) Create(ctx context.Context, cp CreateParams) (Source, error) {
	cfg := cp.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	s, err := scanSource(p.pool.QueryRow(ctx, `
		insert into sources (tenant_id, kind, name, status, config, schedule_cron)
		values ($1, $2::source_kind, $3, 'active', $4, $5)
		returning `+sourceColumns,
		cp.TenantID, cp.Kind, cp.Name, []byte(cfg), cp.ScheduleCron))
	if isUniqueViolation(err) {
		return Source{}, ErrDuplicateName
	}
	return s, err
}

// Update applies a patch to one tenant-scoped source. It builds the SET clause
// from only the provided fields; a (tenant_id, name) collision is ErrDuplicateName
// and a missing row is ErrNotFound.
func (p PoolDB) Update(ctx context.Context, tenantID, id string, patch UpdatePatch) (Source, error) {
	if !validUUID(id) {
		return Source{}, ErrNotFound
	}
	set := "updated_at = now()"
	args := []any{tenantID, id}
	add := func(clause string, val any) {
		args = append(args, val)
		set += ", " + clause + " = $" + strconv.Itoa(len(args))
	}
	if patch.Name != nil {
		add("name", *patch.Name)
	}
	if patch.Config != nil {
		add("config", []byte(*patch.Config))
	}
	if patch.Status != nil {
		args = append(args, *patch.Status)
		set += ", status = $" + strconv.Itoa(len(args)) + "::source_status"
	}
	if patch.ClearSchedule {
		set += ", schedule_cron = null"
	} else if patch.ScheduleCron != nil {
		add("schedule_cron", *patch.ScheduleCron)
	}

	s, err := scanSource(p.pool.QueryRow(ctx,
		`update sources set `+set+` where tenant_id = $1 and id = $2 returning `+sourceColumns, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Source{}, ErrDuplicateName
	}
	return s, err
}

// MarkDeleting flips status to 'deleting'. It reports whether the row changed and
// whether it exists at all, so the service can distinguish 404 from an idempotent
// re-delete without a second round trip on the happy path.
func (p PoolDB) MarkDeleting(ctx context.Context, tenantID, id string) (bool, bool, error) {
	if !validUUID(id) {
		return false, false, nil
	}
	tag, err := p.pool.Exec(ctx,
		`update sources set status = 'deleting', updated_at = now()
		 where tenant_id = $1 and id = $2 and status <> 'deleting'`, tenantID, id)
	if err != nil {
		return false, false, err
	}
	if tag.RowsAffected() == 1 {
		return true, true, nil
	}
	// No change: either already deleting or the row does not exist.
	var exists bool
	if err := p.pool.QueryRow(ctx,
		`select exists(select 1 from sources where tenant_id = $1 and id = $2)`, tenantID, id).Scan(&exists); err != nil {
		return false, false, err
	}
	return false, exists, nil
}

// EnqueueJob writes a queued jobs row (the history/mirror the EPIC-09 worker will
// consume). A sync_source insert that trips the partial unique index
// jobs_one_active_sync_per_source becomes ErrActiveSyncExists (SPEC-07 §2 409).
func (p PoolDB) EnqueueJob(ctx context.Context, nj NewJob) (Job, error) {
	var j Job
	var sid *string
	err := p.pool.QueryRow(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload)
		values ($1, $2, $3::job_kind, 'queued', $4)
		returning id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at`,
		nj.TenantID, nj.SourceID, nj.Kind, []byte(nj.Payload)).
		Scan(&j.ID, &j.TenantID, &sid, &j.Kind, &j.Status, &j.Payload, &j.QueuedAt)
	if isUniqueViolation(err) {
		return Job{}, ErrActiveSyncExists
	}
	if err != nil {
		return Job{}, err
	}
	j.SourceID = sid
	return j, nil
}

// FindActiveSync returns an active sync_source job for the source whose payload
// idempotency_key matches, enabling idempotent replay of POST .../sync.
func (p PoolDB) FindActiveSync(ctx context.Context, tenantID, sourceID, key string) (Job, bool, error) {
	if key == "" || !validUUID(sourceID) {
		return Job{}, false, nil
	}
	var j Job
	var sid *string
	err := p.pool.QueryRow(ctx, `
		select id::text, tenant_id::text, source_id::text, kind::text, status::text, payload, queued_at
		from jobs
		where tenant_id = $1 and source_id = $2 and kind = 'sync_source'
		  and status in ('queued', 'running') and payload->>'idempotency_key' = $3
		order by queued_at desc
		limit 1`,
		tenantID, sourceID, key).
		Scan(&j.ID, &j.TenantID, &sid, &j.Kind, &j.Status, &j.Payload, &j.QueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.SourceID = sid
	return j, true, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint (23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
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
