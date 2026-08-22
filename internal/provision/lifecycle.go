package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lifecycle drives tenant status transitions after provisioning: suspension,
// resumption, scheduled deletion with a grace period, cancellation during grace,
// and the irreversible teardown once the grace period elapses (STORY-02.4,
// FR-TEN-04/05, SPEC-01 §8).
//
// Like the Provisioner it uses a single privileged (superuser) connection: the
// registry writes are control-plane, and the teardown's DROP DATABASE / DROP ROLE
// are superuser-only DDL. It holds no per-request state, so one value is reusable.
//
// Every status write goes through the control plane, so the tenant_changed
// LISTEN/NOTIFY trigger (SPEC-01 §3) fires and the resolver invalidates its cache
// within ~1s — a suspension or deletion takes effect promptly, not on the 30s TTL.
// Each operation records an audit event (C-3: control-plane only).
type Lifecycle struct {
	// PrivilegedURL is the superuser connection to the cluster's admin database
	// (typically the control-plane URL), used for registry writes and the
	// DROP DATABASE / DROP ROLE teardown DDL.
	PrivilegedURL string

	// Encrypter seals a new tenant DB password during a Move (SPEC-09 §2, C-4).
	// It must share a DEK/version with the resolver's Decrypter so the ciphertext
	// it writes stays openable. Only Move needs it; the status transitions and
	// teardown do not touch secrets, so it may be nil for those.
	Encrypter Encrypter

	// Now returns the current time; injectable so tests can drive the grace timer
	// deterministically. Defaults to time.Now.
	Now func() time.Time

	// ObjectStore, if set, removes a tenant's object-storage prefix during
	// teardown (SPEC-01 §8). It is optional: no S3/MinIO client exists in the repo
	// yet, so when nil the teardown skips the prefix removal and records that it
	// was deferred (see ADR-0017). EPIC-06 wires a real client.
	ObjectStore ObjectPrefixDeleter
}

// ObjectPrefixDeleter removes all objects under a tenant's storage prefix. The
// upload/object-storage layer (EPIC-06) provides the concrete implementation;
// Lifecycle depends only on this behaviour so the teardown path is testable and
// so adding storage later needs no change here (NFR-MNT-01).
type ObjectPrefixDeleter interface {
	DeletePrefix(ctx context.Context, tenantID string) error
}

// LifecycleResult reports the outcome of a lifecycle operation.
type LifecycleResult struct {
	TenantID    string
	Slug        string
	FromStatus  string
	ToStatus    string
	DeleteAfter time.Time // set by ScheduleDelete: when the drop becomes eligible
	// ObjectStoreDeferred is true when RunDelete could not remove the object
	// storage prefix because no client was configured (recorded, not silently
	// dropped; ADR-0017).
	ObjectStoreDeferred bool
}

func (l *Lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Lifecycle) precheck(slug string) error {
	if strings.TrimSpace(l.PrivilegedURL) == "" {
		return fmt.Errorf("%w: no privileged connection URL", errValidation)
	}
	if strings.TrimSpace(slug) == "" {
		return fmt.Errorf("%w: slug is required", errValidation)
	}
	return nil
}

// Suspend moves an active tenant to suspended: reads still serve (read-only for
// end users, per SPEC-01 §3), writes and query/ingest for viewers/API keys are
// refused (FR-TEN-04). Admins can still read.
func (l *Lifecycle) Suspend(ctx context.Context, slug string) (LifecycleResult, error) {
	return l.transition(ctx, slug, "suspended", "tenant.suspend")
}

// Resume moves a suspended tenant back to active.
func (l *Lifecycle) Resume(ctx context.Context, slug string) (LifecycleResult, error) {
	return l.transition(ctx, slug, "active", "tenant.resume")
}

// ScheduleDelete moves a tenant to deleting and records the grace deadline
// (now + grace, default 7 days). The tenant is refused by the resolver
// immediately (ErrTenantUnavailable); the data is retained until RunDelete after
// the deadline, so a platform admin can cancel within the grace window
// (FR-TEN-05, SPEC-01 §8). The prior status is stored so CancelDelete can restore
// it. A negative grace is rejected; zero uses the default.
func (l *Lifecycle) ScheduleDelete(ctx context.Context, slug string, grace time.Duration) (LifecycleResult, error) {
	if err := l.precheck(slug); err != nil {
		return LifecycleResult{}, err
	}
	if grace < 0 {
		return LifecycleResult{}, fmt.Errorf("%w: grace period must not be negative", errValidation)
	}
	deadline := graceDeadline(l.now(), grace)

	return l.withAdmin(ctx, func(admin *pgxpool.Pool) (LifecycleResult, error) {
		tx, err := admin.Begin(ctx)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		id, from, err := lockTenant(ctx, tx, slug)
		if err != nil {
			return LifecycleResult{}, err
		}
		if err := validateTransition(from, "deleting"); err != nil {
			return LifecycleResult{}, err
		}

		// Record the deadline and remember the prior status so cancellation can
		// restore exactly the state the tenant was serving in.
		if _, err := tx.Exec(ctx,
			`update tenants
			   set status = 'deleting', delete_after = $2,
			       settings = jsonb_set(coalesce(settings,'{}'), '{delete_prev_status}', to_jsonb($3::text)),
			       updated_at = now()
			 where id = $1`, id, deadline, from); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: schedule delete: %w", err)
		}
		if err := writeAudit(ctx, tx, id, "tenant.delete.schedule",
			fmt.Sprintf(`{"slug":%q,"delete_after":%q,"prev_status":%q}`,
				slug, deadline.UTC().Format(time.RFC3339), from)); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: commit schedule delete: %w", err)
		}
		return LifecycleResult{TenantID: id, Slug: slug, FromStatus: from, ToStatus: "deleting", DeleteAfter: deadline}, nil
	})
}

// CancelDelete aborts a pending deletion during the grace window, restoring the
// status the tenant had before ScheduleDelete (active or suspended) and clearing
// the grace deadline. It fails if the tenant is not currently deleting.
func (l *Lifecycle) CancelDelete(ctx context.Context, slug string) (LifecycleResult, error) {
	if err := l.precheck(slug); err != nil {
		return LifecycleResult{}, err
	}
	return l.withAdmin(ctx, func(admin *pgxpool.Pool) (LifecycleResult, error) {
		tx, err := admin.Begin(ctx)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		id, from, err := lockTenant(ctx, tx, slug)
		if err != nil {
			return LifecycleResult{}, err
		}

		var prev string
		if err := tx.QueryRow(ctx,
			`select coalesce(settings->>'delete_prev_status', 'active') from tenants where id = $1`,
			id).Scan(&prev); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: read prior status: %w", err)
		}
		if prev != "active" && prev != "suspended" {
			prev = "active"
		}
		if err := validateTransition(from, prev); err != nil {
			return LifecycleResult{}, err
		}

		if _, err := tx.Exec(ctx,
			`update tenants
			   set status = $2::tenant_status, delete_after = null,
			       settings = settings - 'delete_prev_status',
			       updated_at = now()
			 where id = $1`, id, prev); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: cancel delete: %w", err)
		}
		if err := writeAudit(ctx, tx, id, "tenant.delete.cancel",
			fmt.Sprintf(`{"slug":%q,"restored_status":%q}`, slug, prev)); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: commit cancel delete: %w", err)
		}
		return LifecycleResult{TenantID: id, Slug: slug, FromStatus: from, ToStatus: prev}, nil
	})
}

// RunDelete performs the irreversible teardown once the grace period has elapsed
// (SPEC-01 §8): drop the tenant database (FORCE-terminating any residual
// sessions), drop the per-tenant role, remove the object-storage prefix, delete
// the tenant_databases row and cascade-delete the control-plane rows, and set the
// tenant deleted with an audit event. It refuses to run before delete_after,
// which is what makes the grace window meaningful.
//
// Ordering matters: DROP DATABASE first (so the role owns nothing and DROP ROLE
// cannot fail on dependent objects), then DROP ROLE, then the registry deletes.
// Each drop is idempotent (IF EXISTS), so a re-run after a partial teardown
// completes cleanly (retry safety for the future River delete_tenant job).
func (l *Lifecycle) RunDelete(ctx context.Context, slug string) (LifecycleResult, error) {
	if err := l.precheck(slug); err != nil {
		return LifecycleResult{}, err
	}
	return l.withAdmin(ctx, func(admin *pgxpool.Pool) (LifecycleResult, error) {
		// Read the tenant, its connection details and the grace deadline.
		var (
			id, from, dbName, role string
			deleteAfter            *time.Time
		)
		err := admin.QueryRow(ctx,
			`select t.id::text, t.status::text, coalesce(d.database_name,''), coalesce(d.username,''),
			        t.delete_after
			 from tenants t left join tenant_databases d on d.tenant_id = t.id
			 where t.slug = $1`, slug).Scan(&id, &from, &dbName, &role, &deleteAfter)
		if errors.Is(err, pgx.ErrNoRows) {
			return LifecycleResult{}, fmt.Errorf("%w: tenant %q not found", errValidation, slug)
		}
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: load tenant: %w", err)
		}
		if from != "deleting" {
			return LifecycleResult{}, fmt.Errorf("%w: tenant %q is %s, not deleting", errValidation, slug, from)
		}
		if deleteAfter == nil {
			return LifecycleResult{}, fmt.Errorf("%w: tenant %q has no grace deadline recorded", errValidation, slug)
		}
		if l.now().Before(*deleteAfter) {
			return LifecycleResult{}, fmt.Errorf("%w: tenant %q grace period not elapsed (delete_after %s)",
				errValidation, slug, deleteAfter.UTC().Format(time.RFC3339))
		}

		// Drop the database, then the role (order matters), both idempotent.
		if dbName != "" {
			stmt, serr := dropDatabaseSQL(dbName)
			if serr != nil {
				return LifecycleResult{}, serr
			}
			if _, derr := admin.Exec(ctx, stmt); derr != nil {
				return LifecycleResult{}, fmt.Errorf("lifecycle: drop database: %w", derr)
			}
		}
		if role != "" {
			stmt, serr := dropRoleSQL(role)
			if serr != nil {
				return LifecycleResult{}, serr
			}
			if _, derr := admin.Exec(ctx, stmt); derr != nil {
				return LifecycleResult{}, fmt.Errorf("lifecycle: drop role: %w", derr)
			}
		}

		// Remove the object-storage prefix (SPEC-01 §8). Deferred when no client
		// is wired (ADR-0017): recorded on the result and in the audit, not lost.
		objectDeferred := true
		if l.ObjectStore != nil {
			if derr := l.ObjectStore.DeletePrefix(ctx, id); derr != nil {
				return LifecycleResult{}, fmt.Errorf("lifecycle: delete object prefix: %w", derr)
			}
			objectDeferred = false
		}

		// Delete registry rows and mark deleted in one transaction. Deleting the
		// tenants row would cascade-delete tenant_databases/members/api_keys/jobs/
		// sources/usage; instead we keep a tombstone tenants row in status deleted
		// (deleted_at set) for audit history, and explicitly clear the child rows so
		// "no rows remain" holds for everything but the tombstone.
		tx, err := admin.Begin(ctx)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: begin teardown tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		for _, table := range []string{
			"tenant_databases", "tenant_members", "api_keys", "sources", "jobs", "usage_daily",
		} {
			if _, derr := tx.Exec(ctx,
				fmt.Sprintf("delete from %s where tenant_id = $1", table), id); derr != nil {
				return LifecycleResult{}, fmt.Errorf("lifecycle: clear %s: %w", table, derr)
			}
		}
		if _, derr := tx.Exec(ctx,
			`update tenants
			   set status = 'deleted', deleted_at = now(), delete_after = null,
			       settings = settings - 'delete_prev_status', updated_at = now()
			 where id = $1`, id); derr != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: mark deleted: %w", derr)
		}
		if err := writeAudit(ctx, tx, id, "tenant.delete.complete",
			fmt.Sprintf(`{"slug":%q,"database":%q,"role":%q,"object_store_deferred":%t}`,
				slug, dbName, role, objectDeferred)); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: commit teardown: %w", err)
		}

		return LifecycleResult{
			TenantID: id, Slug: slug, FromStatus: from, ToStatus: "deleted",
			ObjectStoreDeferred: objectDeferred,
		}, nil
	})
}

// transition is the shared active<->suspended path: lock the tenant, validate the
// move, update status, and audit — all in one transaction.
func (l *Lifecycle) transition(ctx context.Context, slug, to, action string) (LifecycleResult, error) {
	if err := l.precheck(slug); err != nil {
		return LifecycleResult{}, err
	}
	return l.withAdmin(ctx, func(admin *pgxpool.Pool) (LifecycleResult, error) {
		tx, err := admin.Begin(ctx)
		if err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		id, from, err := lockTenant(ctx, tx, slug)
		if err != nil {
			return LifecycleResult{}, err
		}
		if err := validateTransition(from, to); err != nil {
			return LifecycleResult{}, err
		}
		if _, err := tx.Exec(ctx,
			`update tenants set status = $2::tenant_status, updated_at = now() where id = $1`,
			id, to); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: set status %s: %w", to, err)
		}
		if err := writeAudit(ctx, tx, id, action, fmt.Sprintf(`{"slug":%q}`, slug)); err != nil {
			return LifecycleResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LifecycleResult{}, fmt.Errorf("lifecycle: commit %s: %w", to, err)
		}
		return LifecycleResult{TenantID: id, Slug: slug, FromStatus: from, ToStatus: to}, nil
	})
}

// withAdmin opens the privileged pool, pings it (fail fast on an unreachable
// cluster) and runs fn, closing the pool afterwards.
func (l *Lifecycle) withAdmin(ctx context.Context, fn func(*pgxpool.Pool) (LifecycleResult, error)) (LifecycleResult, error) {
	admin, err := pgxpool.New(ctx, l.PrivilegedURL)
	if err != nil {
		return LifecycleResult{}, fmt.Errorf("lifecycle: connect privileged: %w", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		return LifecycleResult{}, fmt.Errorf("lifecycle: reach privileged connection: %w", err)
	}
	return fn(admin)
}

// lockTenant locks the tenants row for the slug FOR UPDATE and returns its id and
// current status, so a concurrent lifecycle operation on the same tenant
// serialises rather than racing.
func lockTenant(ctx context.Context, tx pgx.Tx, slug string) (id, status string, err error) {
	err = tx.QueryRow(ctx,
		`select id::text, status::text from tenants where slug = $1 for update`, slug).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: tenant %q not found", errValidation, slug)
	}
	if err != nil {
		return "", "", fmt.Errorf("lifecycle: lock tenant %q: %w", slug, err)
	}
	return id, status, nil
}

// writeAudit inserts a control-plane audit row (C-3). details is a JSON object
// carrying non-secret metadata only.
func writeAudit(ctx context.Context, tx pgx.Tx, tenantID, action, details string) error {
	if _, err := tx.Exec(ctx,
		`insert into audit_log (tenant_id, action, target_type, target_id, details)
		 values ($1, $2, 'tenant', $1, $3::jsonb)`, tenantID, action, details); err != nil {
		return fmt.Errorf("lifecycle: write audit %s: %w", action, err)
	}
	return nil
}
