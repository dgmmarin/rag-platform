package tenant

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is the tenant lifecycle state, mirroring the control-plane
// tenant_status enum (FR-TEN-03, control_plane.sql).
type Status string

// Tenant lifecycle statuses (FR-TEN-03). They mirror the control-plane
// tenant_status enum and drive the resolution rules in SPEC-01 §3.
const (
	StatusProvisioning Status = "provisioning"
	StatusActive       Status = "active"
	StatusSuspended    Status = "suspended"
	StatusDeleting     Status = "deleting"
	StatusDeleted      Status = "deleted"
)

// Record is the resolved registry view of a tenant: its lifecycle status plus
// the connection details needed to build a pool. It is assembled from the
// control-plane tenants and tenant_databases rows (SPEC-01 §3).
type Record struct {
	ID            ID
	Status        Status
	SchemaVersion int

	// Connection details from tenant_databases. PasswordEnc is the envelope-
	// encrypted password (SPEC-09 §2); it is decrypted only at pool build time
	// and never logged.
	Host        string
	Port        int
	Database    string
	Username    string
	PasswordEnc []byte
	SSLMode     string
	MaxConns    int
}

// DB is the only type that can execute SQL against a tenant's database. Its
// fields are unexported so it can be constructed solely within this package,
// via Resolver.Open (ADR-0003). A read-only DB (suspended tenant) refuses writes.
type DB struct {
	id       ID
	status   Status
	pool     *pgxpool.Pool
	readOnly bool
}

// ID returns the tenant this handle is bound to.
func (d *DB) ID() ID { return d.id }

// Status returns the tenant lifecycle status at the time the handle was opened.
func (d *DB) Status() Status { return d.status }

// ReadOnly reports whether writes are refused (suspended tenant).
func (d *DB) ReadOnly() bool { return d.readOnly }

// Query runs a read query against the tenant database.
func (d *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return d.pool.Query(ctx, sql, args...)
}

// QueryRow runs a read query returning at most one row.
func (d *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return d.pool.QueryRow(ctx, sql, args...)
}

// Exec runs a write. It refuses with ErrReadOnly on a suspended tenant before
// touching the pool (SPEC-01 §3).
func (d *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if d.readOnly {
		return pgconn.CommandTag{}, ErrReadOnly
	}
	return d.pool.Exec(ctx, sql, args...)
}

// Begin starts a transaction. It refuses with ErrReadOnly on a suspended tenant.
func (d *DB) Begin(ctx context.Context) (pgx.Tx, error) {
	if d.readOnly {
		return nil, ErrReadOnly
	}
	return d.pool.Begin(ctx)
}

// Unsafe exposes the raw pool for the migration and provisioning commands only.
// It is forbidden in application code by lint rule (ADR-0003, STORY-02.6).
func (d *DB) Unsafe() *pgxpool.Pool { return d.pool }
