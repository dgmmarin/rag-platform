//go:build e2e

// STORY-02.4 golden path: tenant suspension, deletion and grace period against
// the REAL local stack (Postgres+pgvector via `mise run up`), with NO mocks. It
// provisions a tenant with the real `ragctl enroll`, then drives the lifecycle
// via the real `ragctl tenant …` commands and asserts the effects through the
// real resolver and against the real database (FR-TEN-04/05, SPEC-01 §8):
//   - suspend -> resolver hands back a read-only handle (viewers/API keys are
//     refused writes), while an admin/superuser can still read;
//   - schedule delete -> status deleting, resolver refuses with
//     ErrTenantUnavailable, and delete_after (the grace deadline) is recorded;
//   - cancel during grace -> the drop is aborted and the tenant serves again;
//   - run past grace -> the database and role are dropped and no control-plane
//     rows remain for the tenant except the deleted tombstone (the required
//     verification test);
//   - run before grace -> refused, so the grace window is meaningful.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// runTenant runs `ragctl tenant <args…>` against the local stack and returns its
// combined output and exit code. It uses the control-plane superuser URL as the
// privileged connection (same shape enroll uses).
func runTenant(t *testing.T, ageKey, blob string, args ...string) (out string, exit int) {
	t.Helper()
	bin := buildRagctl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	full := append([]string{"tenant"}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	env := dekEnv()
	for _, k := range []string{"CONTROL_PLANE_URL", "PROVISION_DB_URL"} {
		env = withoutEnv(env, k)
	}
	env = append(env,
		"CONTROL_PLANE_URL="+controlPlaneURL(),
		"PROVISION_DB_URL="+controlPlaneURL(),
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		"DEK_WRAPPED_PATH="+blob,
		"DEK_KEY_VERSION=1",
	)
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	out = string(b)
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("tenant %v: unexpected error %v\n%s", args, err, out)
		}
		exit = exitErr.ExitCode()
	}
	return out, exit
}

// tenantIDForSlug reads the tenant UUID for a slug so the resolver can Open it.
func tenantIDForSlug(t *testing.T, slug string) tenant.ID {
	t.Helper()
	raw := psqlScalar(t, fmt.Sprintf("select id::text from tenants where slug = '%s'", slug))
	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("parse tenant id %q: %v", raw, err)
	}
	return tenant.ID(id)
}

// lifecycleResolver builds a resolver whose Decrypter shares the enroll DEK, so it
// can open passwords the real provisioner encrypted.
func lifecycleResolver(t *testing.T) tenant.Resolver {
	t.Helper()
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return tenant.NewResolver(tenant.Config{
		ControlPool: controlPool(t),
		Decrypter:   cipher,
		CacheTTL:    50 * time.Millisecond, // short so status changes are seen promptly
	})
}

func TestTenantLifecycleGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t) // wraps migrateDEK; enroll + resolver share it

	slug := "life-" + strings.ReplaceAll(mustSuffix(t), "-", "")
	const dim = 768

	// Capture connection details for teardown/verification even if the test fails
	// before the drop.
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		dbName := tryScalar(slug, "d.database_name")
		role := tryScalar(slug, "d.username")
		if dbName != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		}
		if role != "" {
			_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
		}
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE slug = '%s'", slug))
	})

	// --- Provision a real tenant. ---
	if out, exit := runEnroll(t, ageKey, blob, slug, "Lifecycle Co", dim); exit != 0 {
		t.Fatalf("enroll exited %d\n%s", exit, out)
	}
	id := tenantIDForSlug(t, slug)
	dbName := tenantScalar(t, slug, "d.database_name")
	role := tenantScalar(t, slug, "d.username")

	res := lifecycleResolver(t)
	ctx := context.Background()

	// Active: read-write.
	if db, err := res.Open(ctx, id); err != nil || db.ReadOnly() {
		t.Fatalf("active Open: err=%v readOnly=%v", err, db.ReadOnly())
	}

	// --- Suspend: viewers/API keys get read-only; admins can still read. ---
	if out, exit := runTenant(t, ageKey, blob, "suspend", "--slug", slug); exit != 0 {
		t.Fatalf("tenant suspend exited %d\n%s", exit, out)
	}
	if got := tenantScalar(t, slug, "t.status"); got != "suspended" {
		t.Fatalf("status after suspend = %q, want suspended", got)
	}
	res.Close(id) // drop cached pool so the new status is read immediately
	time.Sleep(80 * time.Millisecond)
	sdb, err := res.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open suspended: %v", err)
	}
	if !sdb.ReadOnly() {
		t.Fatal("suspended tenant must resolve read-only (end users refused writes)")
	}
	// An admin/superuser can still read the tenant DB directly (FR-TEN-04).
	execPsql(t, dbName, "select 1")

	// --- Resume, then re-suspend proves the round trip works. ---
	if out, exit := runTenant(t, ageKey, blob, "resume", "--slug", slug); exit != 0 {
		t.Fatalf("tenant resume exited %d\n%s", exit, out)
	}
	if got := tenantScalar(t, slug, "t.status"); got != "active" {
		t.Fatalf("status after resume = %q, want active", got)
	}

	// --- Schedule delete: status deleting, resolver refuses, grace recorded. ---
	if out, exit := runTenant(t, ageKey, blob, "delete", "--slug", slug, "--grace", "168h"); exit != 0 {
		t.Fatalf("tenant delete (schedule) exited %d\n%s", exit, out)
	}
	if got := tenantScalar(t, slug, "t.status"); got != "deleting" {
		t.Fatalf("status after schedule = %q, want deleting", got)
	}
	if got := tenantScalar(t, slug, "t.delete_after is not null"); got != "t" {
		t.Fatal("delete_after not recorded on scheduled deletion")
	}
	res.Close(id)
	time.Sleep(80 * time.Millisecond)
	if _, err := res.Open(ctx, id); !errors.Is(err, tenant.ErrTenantUnavailable) {
		t.Fatalf("Open deleting: want ErrTenantUnavailable, got %v", err)
	}

	// --- Run before grace elapses is refused (grace window is meaningful). ---
	if out, exit := runTenant(t, ageKey, blob, "delete", "--slug", slug, "--run"); exit == 0 {
		t.Fatalf("tenant delete --run before grace should fail; exit 0\n%s", out)
	}
	// Database and role must still exist after the refused run.
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_database where datname = '%s'", dbName)); got != "1" {
		t.Fatal("database dropped despite grace not elapsed")
	}

	// --- Cancel during grace: teardown aborted, tenant serves again. ---
	if out, exit := runTenant(t, ageKey, blob, "delete", "--slug", slug, "--cancel"); exit != 0 {
		t.Fatalf("tenant delete --cancel exited %d\n%s", exit, out)
	}
	if got := tenantScalar(t, slug, "t.status"); got != "active" {
		t.Fatalf("status after cancel = %q, want active (restored)", got)
	}
	if got := tenantScalar(t, slug, "t.delete_after is null"); got != "t" {
		t.Fatal("delete_after not cleared on cancel")
	}
	res.Close(id)
	time.Sleep(80 * time.Millisecond)
	if db, err := res.Open(ctx, id); err != nil || db.ReadOnly() {
		t.Fatalf("Open after cancel: err=%v readOnly=%v (want read-write)", err, db.ReadOnly())
	}
	// The database and role still exist — cancel really aborted the drop.
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_database where datname = '%s'", dbName)); got != "1" {
		t.Fatal("database dropped despite cancellation")
	}
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_roles where rolname = '%s'", role)); got != "1" {
		t.Fatal("role dropped despite cancellation")
	}

	// --- Re-schedule with a tiny grace, let it elapse, then run the teardown. ---
	if out, exit := runTenant(t, ageKey, blob, "delete", "--slug", slug, "--grace", "1s"); exit != 0 {
		t.Fatalf("tenant delete (re-schedule) exited %d\n%s", exit, out)
	}
	time.Sleep(1200 * time.Millisecond) // past the 1s grace deadline
	if out, exit := runTenant(t, ageKey, blob, "delete", "--slug", slug, "--run"); exit != 0 {
		t.Fatalf("tenant delete --run past grace exited %d\n%s", exit, out)
	}

	// Verification: database and role are gone.
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_database where datname = '%s'", dbName)); got != "0" {
		t.Fatalf("database %q still exists after teardown", dbName)
	}
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_roles where rolname = '%s'", role)); got != "0" {
		t.Fatalf("role %q still exists after teardown", role)
	}

	// Verification: no control-plane child rows remain for the tenant (the
	// required "no rows remain" assertion). The tenants tombstone (status
	// deleted) is retained for audit history.
	for _, table := range []string{"tenant_databases", "tenant_members", "api_keys", "sources", "jobs", "usage_daily"} {
		got := psqlScalar(t, fmt.Sprintf(
			"select count(*) from %s where tenant_id = '%s'", table, id))
		if got != "0" {
			t.Fatalf("%d row(s) remain in %s for deleted tenant", parseIntOrZero(got), table)
		}
	}
	if got := tenantScalar2(t, slug, "t.status"); got != "deleted" {
		t.Fatalf("tenant tombstone status = %q, want deleted", got)
	}
	if got := psqlScalar(t, fmt.Sprintf("select t.deleted_at is not null from tenants t where slug = '%s'", slug)); got != "t" {
		t.Fatal("deleted_at not set on the tombstone")
	}

	// The teardown wrote a completion audit event (C-3: control-plane only).
	if got := psqlScalar(t, fmt.Sprintf(
		"select count(*) from audit_log a join tenants t on t.id = a.tenant_id "+
			"where t.slug = '%s' and a.action = 'tenant.delete.complete'", slug)); got == "0" {
		t.Fatal("no tenant.delete.complete audit event written")
	}

	// The resolver now reports the tenant as not found (deleted).
	res.Close(id)
	time.Sleep(80 * time.Millisecond)
	if _, err := res.Open(ctx, id); !errors.Is(err, tenant.ErrTenantNotFound) {
		t.Fatalf("Open deleted: want ErrTenantNotFound, got %v", err)
	}
}

// tenantScalar2 reads a tenants-only scalar by slug (no join), used after the
// tenant_databases row has been deleted by teardown.
func tenantScalar2(t *testing.T, slug, expr string) string {
	t.Helper()
	return psqlScalar(t, fmt.Sprintf("select %s from tenants t where t.slug = '%s'", expr, slug))
}

// parseIntOrZero parses a psql count, defaulting to 0 for readable failure output.
func parseIntOrZero(s string) int {
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}
