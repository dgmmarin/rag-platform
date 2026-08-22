//go:build e2e

// STORY-02.1 golden path: the tenant Resolver, against the REAL local stack
// (Postgres+pgvector via `mise run up`), resolves a tenant to a working *DB with
// the correct read/write mode for its lifecycle status (SPEC-01 §3), reusing a
// per-tenant pool (SPEC-01 §4). No mocks: a real per-tenant role + database is
// created, real control-plane rows are inserted, and real SQL is executed
// through the handle the resolver hands back.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// testDEK is a fixed 32-byte DEK; the resolver only needs a Decrypter, so no KMS
// wiring is required for the e2e. It never leaves the test process.
var testDEK = []byte("0123456789abcdef0123456789abcdef")

// execPsql runs a statement in the postgres container as the superuser against
// the named database, failing the test on error.
func execPsql(t *testing.T, database, stmt string) {
	t.Helper()
	user := hostPort("POSTGRES_USER", "rag")
	runPsql(t, user, database, stmt)
}

// setupTenantDatabase creates a dedicated role + database for a tenant on the
// real Postgres (a stand-in for STORY-02.3 provisioning) and returns the role
// name and password. The role owns its database and cannot reach any other.
func setupTenantDatabase(t *testing.T, slug string) (dbName, role, password string) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	dbName = "tenant_" + slug + "_" + suffix
	role = "role_" + slug + "_" + suffix
	password = "pw_" + suffix

	// Create the least-privilege role and its database, owned by the role.
	execPsql(t, "control_plane", fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD '%s'", role, password))
	execPsql(t, "control_plane", fmt.Sprintf(
		"CREATE DATABASE %s OWNER %s", dbName, role))

	t.Cleanup(func() {
		// Drop the database then the role; ignore errors on teardown.
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
	})
	return dbName, role, password
}

// insertTenantRows writes the control-plane registry rows the resolver reads: a
// tenants row with the given status and a tenant_databases row with the
// envelope-encrypted password (SPEC-09).
func insertTenantRows(t *testing.T, id tenant.ID, slug, status, dbName, role, password string, schemaVersion int) {
	t.Helper()
	cipher, err := crypto.NewCipher(1, testDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	enc, err := cipher.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}

	execPsql(t, "control_plane", fmt.Sprintf(
		"INSERT INTO tenants (id, slug, name, status) VALUES ('%s', '%s', '%s', '%s')",
		id, slug, slug, status))
	// The resolver runs as a host process, so it must reach Postgres via the
	// published host:port (localhost:<mapped>), not the in-network container name.
	port := hostPort("POSTGRES_PORT", "5432")
	execPsql(t, "control_plane", fmt.Sprintf(
		"INSERT INTO tenant_databases (tenant_id, host, port, database_name, username, password_enc, ssl_mode, schema_version, max_conns) "+
			"VALUES ('%s', 'localhost', %s, '%s', '%s', decode('%x','hex'), 'disable', %d, 5)",
		id, port, dbName, role, enc, schemaVersion))

	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", id))
	})
}

// controlPool opens a pgxpool to the control-plane database from the host.
func controlPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, controlPlaneURL())
	if err != nil {
		t.Fatalf("control pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestResolver(t *testing.T) tenant.Resolver {
	t.Helper()
	cipher, err := crypto.NewCipher(1, testDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return tenant.NewResolver(tenant.Config{
		ControlPool: controlPool(t),
		Decrypter:   cipher,
		CacheTTL:    50 * time.Millisecond, // short so the status-change assertion is fast
	})
}

// TestResolverGoldenPath is the STORY-02.1 golden path across the lifecycle rules.
func TestResolverGoldenPath(t *testing.T) {
	// Ensure the control-plane schema exists (idempotent) via the real binary.
	migrateControl(t)

	res := newTestResolver(t)
	ctx := context.Background()

	// --- Active tenant: read-write handle that actually reads and writes. ---
	activeID := tenant.ID(uuid.New())
	dbName, role, password := setupTenantDatabase(t, "active")
	insertTenantRows(t, activeID, "active-"+dbName, string(tenant.StatusActive), dbName, role, password, 1)
	// Give the tenant a table to write to (as its own role, so ownership is real).
	execPsqlAs(t, role, password, dbName, "CREATE TABLE notes (body text)")

	adb, err := res.Open(ctx, activeID)
	if err != nil {
		t.Fatalf("Open active: %v", err)
	}
	if adb.ReadOnly() {
		t.Fatal("active tenant must be read-write")
	}
	if _, err := adb.Exec(ctx, "INSERT INTO notes (body) VALUES ($1)", "hello"); err != nil {
		t.Fatalf("write via active DB: %v", err)
	}
	var body string
	if err := adb.QueryRow(ctx, "SELECT body FROM notes").Scan(&body); err != nil {
		t.Fatalf("read via active DB: %v", err)
	}
	if body != "hello" {
		t.Fatalf("read back %q, want %q", body, "hello")
	}

	// Pool reuse: a second Open returns a handle bound to the same tenant DB.
	// (Pointer-level pool identity across Opens is asserted by the internal unit
	// test TestPoolCacheLazyCreateAndReuse; reaching the raw pool via Unsafe()
	// here is forbidden by the ADR-0003 lint rule, STORY-02.6.) A row written via
	// the first handle is immediately visible via the second, proving both handles
	// route to the same database (SPEC-01 §4).
	adb2, err := res.Open(ctx, activeID)
	if err != nil {
		t.Fatalf("second Open active: %v", err)
	}
	if _, err := adb.Exec(ctx, "INSERT INTO notes (body) VALUES ($1)", "reuse-probe"); err != nil {
		t.Fatalf("write via first handle: %v", err)
	}
	var seen int
	if err := adb2.QueryRow(ctx, "SELECT count(*) FROM notes WHERE body = $1", "reuse-probe").Scan(&seen); err != nil {
		t.Fatalf("read via second handle: %v", err)
	}
	if seen != 1 {
		t.Fatalf("second Open handle does not see the first handle's write; got %d rows (SPEC-01 §4)", seen)
	}

	// --- Suspended tenant: read-only handle (reads ok, writes refused). ---
	suspID := tenant.ID(uuid.New())
	sName, sRole, sPass := setupTenantDatabase(t, "susp")
	insertTenantRows(t, suspID, "susp-"+sName, string(tenant.StatusSuspended), sName, sRole, sPass, 1)
	execPsqlAs(t, sRole, sPass, sName, "CREATE TABLE notes (body text); INSERT INTO notes VALUES ('x')")

	sdb, err := res.Open(ctx, suspID)
	if err != nil {
		t.Fatalf("Open suspended: %v", err)
	}
	if !sdb.ReadOnly() {
		t.Fatal("suspended tenant must be read-only")
	}
	if err := sdb.QueryRow(ctx, "SELECT body FROM notes").Scan(&body); err != nil {
		t.Fatalf("read via suspended DB should be allowed: %v", err)
	}
	if _, err := sdb.Exec(ctx, "INSERT INTO notes VALUES ('y')"); !errors.Is(err, tenant.ErrReadOnly) {
		t.Fatalf("write via suspended DB: want ErrReadOnly, got %v", err)
	}

	// --- Deleted tenant: not found. ---
	delID := tenant.ID(uuid.New())
	dName, dRole, dPass := setupTenantDatabase(t, "del")
	insertTenantRows(t, delID, "del-"+dName, string(tenant.StatusDeleted), dName, dRole, dPass, 1)
	if _, err := res.Open(ctx, delID); !errors.Is(err, tenant.ErrTenantNotFound) {
		t.Fatalf("Open deleted: want ErrTenantNotFound, got %v", err)
	}

	// --- Unknown tenant: not found. ---
	if _, err := res.Open(ctx, tenant.ID(uuid.New())); !errors.Is(err, tenant.ErrTenantNotFound) {
		t.Fatalf("Open unknown: want ErrTenantNotFound, got %v", err)
	}
}

// TestResolverReflectsStatusChangeAfterCacheExpiry proves suspension takes effect
// once the registry cache expires (SPEC-01 §3). The active handle keeps working
// until then; after expiry a fresh Open yields a read-only handle.
func TestResolverReflectsStatusChangeAfterCacheExpiry(t *testing.T) {
	migrateControl(t)
	res := newTestResolver(t)
	ctx := context.Background()

	id := tenant.ID(uuid.New())
	dbName, role, password := setupTenantDatabase(t, "flip")
	insertTenantRows(t, id, "flip-"+dbName, string(tenant.StatusActive), dbName, role, password, 1)

	db, err := res.Open(ctx, id)
	if err != nil || db.ReadOnly() {
		t.Fatalf("Open active: err=%v readOnly=%v", err, db.ReadOnly())
	}

	// Suspend in the control plane, then wait past the (short) cache TTL.
	execPsql(t, "control_plane", fmt.Sprintf(
		"UPDATE tenants SET status = 'suspended' WHERE id = '%s'", id))
	time.Sleep(150 * time.Millisecond)

	db2, err := res.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after suspend: %v", err)
	}
	if !db2.ReadOnly() {
		t.Fatal("after cache expiry the tenant must resolve read-only")
	}
}

// TestResolverNotifyInvalidatesCache proves the LISTEN/NOTIFY path (SPEC-01 §3):
// with a long cache TTL, a suspension is reflected only because the control-plane
// tenants trigger publishes on `tenant_changed` and the resolver's Listen loop
// invalidates the cached record. Without NOTIFY the change would be invisible for
// the TTL.
func TestResolverNotifyInvalidatesCache(t *testing.T) {
	migrateControl(t)

	// Long TTL so any timely change must come from NOTIFY, not TTL expiry.
	cipher, err := crypto.NewCipher(1, testDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	res := tenant.NewResolver(tenant.Config{
		ControlPool: controlPool(t),
		Decrypter:   cipher,
		CacheTTL:    10 * time.Minute,
	})

	// Start the LISTEN loop (exposed via an optional Listener interface).
	listener, ok := res.(interface{ Listen(context.Context) })
	if !ok {
		t.Fatal("resolver does not expose Listen; NOTIFY invalidation unavailable")
	}
	lctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go listener.Listen(lctx)
	time.Sleep(200 * time.Millisecond) // let LISTEN register

	ctx := context.Background()
	id := tenant.ID(uuid.New())
	dbName, role, password := setupTenantDatabase(t, "notify")
	insertTenantRows(t, id, "notify-"+dbName, string(tenant.StatusActive), dbName, role, password, 1)

	// Prime the cache with the active status.
	if db, err := res.Open(ctx, id); err != nil || db.ReadOnly() {
		t.Fatalf("prime Open: err=%v readOnly=%v", err, db.ReadOnly())
	}

	// Suspend; the trigger fires NOTIFY, the Listen loop invalidates the cache.
	execPsql(t, "control_plane", fmt.Sprintf(
		"UPDATE tenants SET status = 'suspended' WHERE id = '%s'", id))

	// Poll for the change to land, well within the 10-minute TTL.
	deadline := time.Now().Add(10 * time.Second)
	for {
		db, err := res.Open(ctx, id)
		if err != nil {
			t.Fatalf("Open during poll: %v", err)
		}
		if db.ReadOnly() {
			return // NOTIFY-driven invalidation worked
		}
		if time.Now().After(deadline) {
			t.Fatal("tenant still read-write 10s after suspend; NOTIFY invalidation did not fire")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
