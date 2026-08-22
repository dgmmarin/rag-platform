package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MoveParams describes the new connection details to record for a tenant during
// a move (FR-TEN-07, SPEC-01 §4). Every field is optional; a zero field leaves
// the existing value unchanged, so an operator can repoint just the host, or
// rotate only the password, without restating the whole record. At least one
// field must be set — an all-zero MoveParams is rejected (a move that changes
// nothing is a caller error, not a silent no-op update).
//
// A move only updates the control-plane connection record; it does NOT copy the
// tenant's data. The physical database copy/restore to the new host is performed
// out of band by the operator (see docs/runbooks/move-tenant.md); repointing the
// registry is what this method does. Because the write goes through the control
// plane, the tenant_changed trigger (SPEC-01 §3) fires and the resolver evicts
// the pool and invalidates its cached record within ~1s, so the next Open
// rebuilds against the new connection (SPEC-01 §4).
type MoveParams struct {
	Host     string // new database host
	Port     int    // new database port (must be > 0 if set)
	Database string // new database name
	Username string // new per-tenant role
	SSLMode  string // new sslmode (e.g. require, verify-full, disable)
	// Password, if set, is the new plaintext password for the tenant role on the
	// new host. It is envelope-encrypted before being stored and is never logged
	// or returned (SPEC-09 §2, C-4). Left empty, the existing encrypted password
	// is kept (a host-only move that reuses the same credentials).
	Password string
}

// isEmpty reports whether the params would change nothing.
func (p MoveParams) isEmpty() bool {
	return p.Host == "" && p.Port == 0 && p.Database == "" &&
		p.Username == "" && p.SSLMode == "" && p.Password == ""
}

// Move updates a tenant's stored database connection details (FR-TEN-07). Only
// the fields set in params change; the password, if supplied, is re-encrypted
// with the platform Cipher so it round-trips through the resolver's decrypt
// (SPEC-09 §2). The update runs in one control-plane transaction that also
// writes an audit event (C-3: connection metadata only, never the password),
// and it fires tenant_changed so the resolver rebuilds against the new
// connection on the next Open.
//
// It does not move data: the operator copies the database to the new host out of
// band first (runbook), then calls Move to repoint the registry.
func (l *Lifecycle) Move(ctx context.Context, slug string, params MoveParams) (LifecycleResult, error) {
	if err := l.precheck(slug); err != nil {
		return LifecycleResult{}, err
	}
	if params.isEmpty() {
		return LifecycleResult{}, fmt.Errorf("%w: move requires at least one connection field to change", errValidation)
	}
	if params.Port < 0 {
		return LifecycleResult{}, fmt.Errorf("%w: port must be positive", errValidation)
	}
	if strings.TrimSpace(params.Password) != "" && l.Encrypter == nil {
		return LifecycleResult{}, fmt.Errorf("%w: no encrypter configured to seal the new password", errValidation)
	}

	// Encrypt the new password up front (before dialing) so a crypto misconfig
	// fails closed without a partial write.
	var encPassword []byte
	if params.Password != "" {
		var err error
		encPassword, err = l.Encrypter.Encrypt([]byte(params.Password))
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("provision: encrypt new password: %w", err)
		}
	}

	return l.withAdmin(ctx, func(admin *pgxpool.Pool) (LifecycleResult, error) {
		tx, err := admin.Begin(ctx)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: begin move tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// Lock the tenant so a concurrent lifecycle op on the same tenant
		// serialises rather than racing.
		id, status, err := lockTenant(ctx, tx, slug)
		if err != nil {
			return LifecycleResult{}, err
		}

		// The move only makes sense for a tenant that has a connection record.
		var exists bool
		if err := tx.QueryRow(ctx,
			`select true from tenant_databases where tenant_id = $1 for update`, id).Scan(&exists); err != nil {
			return LifecycleResult{}, fmt.Errorf("%w: tenant %q has no connection record to move", errValidation, slug)
		}

		// Update only the fields the operator supplied; COALESCE keeps the rest.
		// A nil password argument leaves password_enc unchanged.
		var passwordArg any
		if encPassword != nil {
			passwordArg = encPassword
		}
		if _, err := tx.Exec(ctx,
			`update tenant_databases
			   set host          = coalesce(nullif($2,''), host),
			       port          = coalesce(nullif($3,0),  port),
			       database_name = coalesce(nullif($4,''), database_name),
			       username      = coalesce(nullif($5,''), username),
			       ssl_mode      = coalesce(nullif($6,''), ssl_mode),
			       password_enc  = coalesce($7, password_enc),
			       updated_at    = now()
			 where tenant_id = $1`,
			id, params.Host, params.Port, params.Database, params.Username, params.SSLMode, passwordArg); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: update connection: %w", err)
		}

		// Audit the move with the (non-secret) connection metadata that changed,
		// and whether the password was rotated — never the password itself.
		details := fmt.Sprintf(
			`{"slug":%q,"host":%q,"port":%d,"database":%q,"username":%q,"ssl_mode":%q,"password_rotated":%t}`,
			slug, params.Host, params.Port, params.Database, params.Username, params.SSLMode, encPassword != nil)
		if err := writeAudit(ctx, tx, id, "tenant.move", details); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: commit move: %w", err)
		}

		return LifecycleResult{TenantID: id, Slug: slug, FromStatus: status, ToStatus: status}, nil
	})
}
