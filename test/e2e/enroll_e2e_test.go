//go:build e2e

// STORY-02.3 golden path: `ragctl enroll` provisions a tenant end to end against
// the REAL local stack (Postgres+pgvector via `mise run up`), with NO mocks:
//   - creates a least-privilege role + dedicated database on the cluster,
//   - installs the superuser-only extensions (vector/pgcrypto/pg_trgm) the tenant
//     schema assumes (SPEC-01 §6, ADR-0015),
//   - encrypts the generated password with the platform DEK and records the
//     connection details in the control plane (SPEC-09 §2),
//   - applies the tenant migrations as the per-tenant role (schema present at the
//     configured embedding dimension), and sets the tenant active (FR-TEN-01/02),
//   - writes a provisioning audit event.
//
// It then proves the stored password round-trips (decrypt with the same DEK, log
// in as the role with it) and that re-running enroll is idempotent.
package e2e

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
)

// runEnroll runs the real ragctl binary to provision a tenant and returns its
// combined output and exit code. The privileged connection (PROVISION_DB_URL) is
// the control-plane superuser URL; the tenant DB is recorded at the host-mapped
// Postgres port so a host process (the resolver) can reach it.
func runEnroll(t *testing.T, ageKey, blob, slug, name string, dim int) (out string, exit int) {
	t.Helper()
	bin := buildRagctl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	port := hostPort("POSTGRES_PORT", "5432")
	cmd := exec.CommandContext(ctx, bin, "enroll",
		"--slug", slug, "--name", name, "--region", "eu-central",
		"--db-ssl-mode", "disable",
		"--embedding-dim", fmt.Sprintf("%d", dim))
	env := dekEnv()
	for _, k := range []string{"CONTROL_PLANE_URL", "PROVISION_DB_URL", "TENANT_DB_HOST", "TENANT_DB_PORT"} {
		env = withoutEnv(env, k)
	}
	env = append(env,
		"CONTROL_PLANE_URL="+controlPlaneURL(),
		"PROVISION_DB_URL="+controlPlaneURL(),
		"TENANT_DB_HOST=localhost",
		"TENANT_DB_PORT="+port,
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
			t.Fatalf("enroll: unexpected error %v\n%s", err, out)
		}
		exit = exitErr.ExitCode()
	}
	return out, exit
}

// tenantScalar reads a single control-plane scalar keyed by tenant slug.
func tenantScalar(t *testing.T, slug, expr string) string {
	t.Helper()
	return psqlScalar(t, fmt.Sprintf(
		"select %s from tenants t join tenant_databases d on d.tenant_id = t.id where t.slug = '%s'",
		expr, slug))
}

func TestEnrollProvisionsTenantGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)

	slug := "enroll-" + strings.ReplaceAll(mustSuffix(t), "-", "")
	const dim = 768 // non-default to prove the placeholder is substituted

	t.Cleanup(func() {
		// Best-effort teardown: drop the provisioned DB + role and the registry row.
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

	// --- First enroll: the full provisioning path. ---
	out, exit := runEnroll(t, ageKey, blob, slug, "Enroll Test Co", dim)
	if exit != 0 {
		t.Fatalf("enroll exited %d\n%s", exit, out)
	}

	// Tenant is active with the recorded connection details and mirrored version.
	if status := tenantScalar(t, slug, "t.status"); status != "active" {
		t.Fatalf("tenant status = %q, want active\n%s", status, out)
	}
	dbName := tenantScalar(t, slug, "d.database_name")
	role := tenantScalar(t, slug, "d.username")
	if dbName == "" || role == "" {
		t.Fatalf("tenant_databases row missing (db=%q role=%q)", dbName, role)
	}
	if v := tenantScalar(t, slug, "d.schema_version"); v != "1" {
		t.Fatalf("mirrored schema_version = %q, want 1", v)
	}

	// The role and database really exist on the cluster.
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_roles where rolname = '%s'", role)); got != "1" {
		t.Fatalf("provisioned role %q not found", role)
	}
	if got := psqlScalar(t, fmt.Sprintf("select count(*) from pg_database where datname = '%s'", dbName)); got != "1" {
		t.Fatalf("provisioned database %q not found", dbName)
	}

	// The three required extensions were installed by the privileged connection
	// (SPEC-01 §6): a tenant migration could not have created them.
	for _, ext := range []string{"vector", "pgcrypto", "pg_trgm"} {
		if got := psqlScalar2(t, dbName,
			fmt.Sprintf("select count(*) from pg_extension where extname = '%s'", ext)); got != "1" {
			t.Fatalf("extension %q not installed in tenant DB %s", ext, dbName)
		}
	}

	// Migrations were applied at the configured embedding dimension, and goose
	// recorded the version inside the tenant DB.
	if got := psqlScalar2(t, dbName, "select to_regclass('public.chunks') is not null"); got != "t" {
		t.Fatalf("chunks table missing in provisioned tenant DB")
	}
	if got := psqlScalar2(t, dbName,
		"select a.atttypmod from pg_attribute a join pg_class c on c.oid=a.attrelid "+
			"where c.relname='chunks' and a.attname='embedding'"); got != fmt.Sprintf("%d", dim) {
		t.Fatalf("embedding dimension = %q, want %d (placeholder substitution)", got, dim)
	}
	if got := psqlScalar2(t, dbName, "select max(version_id) from goose_db_version"); got != "1" {
		t.Fatalf("goose version in tenant DB = %q, want 1", got)
	}

	// An audit event was written for the provisioning (C-3: control-plane only).
	if got := psqlScalar(t, fmt.Sprintf(
		"select count(*) from audit_log a join tenants t on t.id = a.tenant_id "+
			"where t.slug = '%s' and a.action in ('tenant.create','tenant.provision')", slug)); got == "0" {
		t.Fatalf("no provisioning audit event written for %q", slug)
	}

	// The stored password round-trips: decrypt the ciphertext with the same DEK
	// the binary used, then log in as the role with it against the tenant DB.
	encHex := psqlScalar(t, fmt.Sprintf(
		"select encode(d.password_enc, 'hex') from tenant_databases d "+
			"join tenants t on t.id = d.tenant_id where t.slug = '%s'", slug))
	password := decryptHex(t, encHex)
	if strings.Contains(out, password) {
		t.Fatal("enroll output leaked the tenant password")
	}
	execPsqlAs(t, role, password, dbName, "select 1")

	// --- Idempotency: a second enroll for the same slug must succeed and leave
	// the tenant active with the same database/role (retry safety). ---
	out2, exit2 := runEnroll(t, ageKey, blob, slug, "Enroll Test Co", dim)
	if exit2 != 0 {
		t.Fatalf("second enroll exited %d\n%s", exit2, out2)
	}
	if status := tenantScalar(t, slug, "t.status"); status != "active" {
		t.Fatalf("after re-enroll status = %q, want active", status)
	}
	if got := tenantScalar(t, slug, "d.database_name"); got != dbName {
		t.Fatalf("re-enroll changed database name: %q -> %q", dbName, got)
	}
	if got := tenantScalar(t, slug, "d.username"); got != role {
		t.Fatalf("re-enroll changed role: %q -> %q", role, got)
	}
	// The password is unchanged, so it must still log in.
	enc2 := psqlScalar(t, fmt.Sprintf(
		"select encode(d.password_enc, 'hex') from tenant_databases d "+
			"join tenants t on t.id = d.tenant_id where t.slug = '%s'", slug))
	if enc2 != encHex {
		t.Fatal("re-enroll regenerated the tenant password; must reuse the stored one")
	}
	execPsqlAs(t, role, decryptHex(t, enc2), dbName, "select 1")
}

// decryptHex decrypts a hex-encoded ciphertext with the shared migrate DEK,
// proving the encrypt side (provisioning) matches the decrypt side (resolver).
func decryptHex(t *testing.T, hexStr string) string {
	t.Helper()
	raw := hexDecode(t, hexStr)
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	pt, err := cipher.Decrypt(raw)
	if err != nil {
		t.Fatalf("decrypt stored password: %v", err)
	}
	return string(pt)
}

// tryScalar reads a control-plane scalar (tuples-only) without failing the test
// (teardown use).
func tryScalar(slug, expr string) string {
	user := hostPort("POSTGRES_USER", "rag")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	query := fmt.Sprintf(
		"select %s from tenants t join tenant_databases d on d.tenant_id = t.id where t.slug = '%s'",
		expr, slug)
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", user, "-d", "control_plane", "-tAc", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// mustSuffix returns a short unique lowercase token for building a unique slug.
func mustSuffix(t *testing.T) string {
	t.Helper()
	return uuid.NewString()[:8]
}

// hexDecode decodes a hex string, failing the test on error.
func hexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}
