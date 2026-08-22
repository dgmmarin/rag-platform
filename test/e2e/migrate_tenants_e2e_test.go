//go:build e2e

// STORY-02.2 golden path: `ragctl migrate tenants` applies the tenant goose
// migrations to every eligible tenant against the REAL local stack
// (Postgres+pgvector via `mise run up`), records each applied schema_version in
// the control plane, continues past a deliberately-failing tenant, exits
// non-zero listing it, and a rerun resumes only the tenant left behind
// (FR-TEN-09, SPEC-01 §7). No mocks: real per-tenant roles/databases, real
// migrations, real SQL asserted through psql.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// migrateDEK is the fixed 32-byte DEK the migrate-tenants e2e uses to both wrap
// the DEK for the ragctl binary and encrypt tenant passwords in the control
// plane, so the binary can decrypt them (SPEC-09 §2). It never leaves the test.
var migrateDEK = bytes.Repeat([]byte{0x24}, 32)

// writeWrappedDEK wraps migrateDEK under a fresh age identity and writes the blob,
// returning the age secret key and blob path for the child process environment.
func writeWrappedDEK(t *testing.T) (ageKey, blobPath string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := w.Write(migrateDEK); err != nil {
		t.Fatalf("write dek: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	blobPath = filepath.Join(t.TempDir(), "dek.age")
	if err := os.WriteFile(blobPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return id.String(), blobPath
}

// provisionExtensions installs the required extensions in a tenant database as
// the superuser. This mirrors SPEC-01 §6 provisioning: extensions are created by
// the privileged connection at database-create time, because tenant migrations
// run as the least-privilege per-tenant role which cannot CREATE EXTENSION. It
// stands in for STORY-02.3 provisioning, which this story predates.
func provisionExtensions(t *testing.T, database string) {
	t.Helper()
	execPsql(t, database, "create extension if not exists vector; create extension if not exists pgcrypto; create extension if not exists pg_trgm")
}

// insertTenantWithDim writes the control-plane rows for a tenant whose password
// is encrypted with migrateDEK and whose settings pin the embedding dimension.
func insertTenantWithDim(t *testing.T, id tenant.ID, slug, dbName, role, password string, dim int) {
	t.Helper()
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	enc, err := cipher.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}

	execPsql(t, "control_plane", fmt.Sprintf(
		"INSERT INTO tenants (id, slug, name, status, settings) VALUES ('%s', '%s', '%s', 'active', '{\"embedding_dim\": %d}')",
		id, slug, slug, dim))
	port := hostPort("POSTGRES_PORT", "5432")
	execPsql(t, "control_plane", fmt.Sprintf(
		"INSERT INTO tenant_databases (tenant_id, host, port, database_name, username, password_enc, ssl_mode, schema_version, max_conns) "+
			"VALUES ('%s', 'localhost', %s, '%s', '%s', decode('%x','hex'), 'disable', 0, 5)",
		id, port, dbName, role, enc))

	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", id))
	})
}

// runMigrateTenants runs the real binary and returns its combined output and
// exit code.
func runMigrateTenants(t *testing.T, ageKey, blob string) (out string, exit int) {
	t.Helper()
	bin := buildRagctl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "migrate", "tenants", "--parallel", "4")
	// Start from a clean env: strip any ambient CONTROL_PLANE_URL and DEK vars so
	// only the values set here are visible to the child (deterministic).
	env := withoutEnv(dekEnv(), "CONTROL_PLANE_URL")
	env = append(env,
		"CONTROL_PLANE_URL="+controlPlaneURL(),
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
			t.Fatalf("migrate tenants: unexpected error %v\n%s", err, out)
		}
		exit = exitErr.ExitCode()
	}
	return out, exit
}

func schemaVersionOf(t *testing.T, id tenant.ID) string {
	t.Helper()
	return psqlScalar(t, fmt.Sprintf(
		"select schema_version from tenant_databases where tenant_id = '%s'", id))
}

// TestMigrateTenantsGoldenPath drives the full STORY-02.2 contract end to end.
func TestMigrateTenantsGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)

	// --- A healthy tenant (dim 768 to prove the placeholder is substituted). ---
	goodID := tenant.ID(uuid.New())
	goodDB, goodRole, goodPass := setupTenantDatabase(t, "good")
	provisionExtensions(t, goodDB)
	goodSlug := "good-" + goodDB
	insertTenantWithDim(t, goodID, goodSlug, goodDB, goodRole, goodPass, 768)

	// --- A deliberately-failing tenant: its registry row points at a database
	// that does not exist, so goose cannot apply migrations. The run must record
	// the failure, keep going, and exit non-zero (SPEC-01 §7). ---
	badID := tenant.ID(uuid.New())
	_, badRole, badPass := setupTenantDatabase(t, "bad")
	badSlug := "bad-" + badRole
	missingDB := "tenant_missing_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	insertTenantWithDim(t, badID, badSlug, missingDB, badRole, badPass, 1536)

	// First run: good succeeds, bad fails, command exits non-zero.
	out, exit := runMigrateTenants(t, ageKey, blob)
	if exit == 0 {
		t.Fatalf("migrate tenants should exit non-zero when a tenant fails\n%s", out)
	}
	if !strings.Contains(out, badSlug) {
		t.Fatalf("failure output must name the failed tenant %q\n%s", badSlug, out)
	}
	if strings.Contains(out, goodPass) || strings.Contains(out, badPass) {
		t.Fatalf("migrate tenants leaked a tenant password in its output")
	}

	// The good tenant advanced to the expected schema version, mirrored to the
	// control plane; the bad tenant stayed at 0 (fail closed, resumable).
	if got := schemaVersionOf(t, goodID); got != "1" {
		t.Fatalf("good tenant schema_version = %q, want 1", got)
	}
	if got := schemaVersionOf(t, badID); got != "0" {
		t.Fatalf("bad tenant schema_version = %q, want 0 (unchanged after failure)", got)
	}

	// The good tenant's database really has the migrated schema at the right
	// embedding dimension, and goose recorded the version there too.
	if got := psqlScalar2(t, goodDB, "select to_regclass('public.chunks') is not null"); got != "t" {
		t.Fatalf("good tenant missing chunks table after migrate")
	}
	if got := psqlScalar2(t, goodDB,
		"select a.atttypmod from pg_attribute a join pg_class c on c.oid=a.attrelid "+
			"where c.relname='chunks' and a.attname='embedding'"); got != "768" {
		t.Fatalf("good tenant embedding dimension = %q, want 768 (placeholder substitution)", got)
	}
	if got := psqlScalar2(t, goodDB, "select max(version_id) from goose_db_version"); got != "1" {
		t.Fatalf("good tenant goose version = %q, want 1", got)
	}

	// --- Rerun resumes: fix the previously-failed tenant's connection to a fresh
	// real database, rerun, and it should now succeed with no double-apply on the
	// good one (SPEC-01 §7: rerun resumes only those behind). ---
	fixedDB, fixedRole, fixedPass := setupTenantDatabase(t, "fixed")
	provisionExtensions(t, fixedDB)
	execPsql(t, "control_plane", fmt.Sprintf(
		"UPDATE tenant_databases SET database_name = '%s', username = '%s', password_enc = decode('%x','hex') WHERE tenant_id = '%s'",
		fixedDB, fixedRole, mustEncrypt(t, fixedPass), badID))

	out2, exit2 := runMigrateTenants(t, ageKey, blob)
	if exit2 != 0 {
		t.Fatalf("rerun after fixing the failed tenant should exit 0\n%s", out2)
	}
	if got := schemaVersionOf(t, badID); got != "1" {
		t.Fatalf("previously-failed tenant schema_version = %q after rerun, want 1", got)
	}
	if got := schemaVersionOf(t, goodID); got != "1" {
		t.Fatalf("good tenant schema_version drifted to %q on rerun, want 1", got)
	}
}

// mustEncrypt encrypts a tenant password under migrateDEK and returns the raw
// ciphertext bytes as a hex-encodable []byte for embedding in SQL.
func mustEncrypt(t *testing.T, password string) []byte {
	t.Helper()
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	enc, err := cipher.Encrypt([]byte(password))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return enc
}

// psqlScalar2 runs a single-value query inside the postgres container against an
// arbitrary database (the per-tenant one), returning the trimmed result.
func psqlScalar2(t *testing.T, database, query string) string {
	t.Helper()
	user := hostPort("POSTGRES_USER", "rag")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", user, "-d", database, "-tAc", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql (%s) %q failed: %v\n%s", database, query, err, out)
	}
	return strings.TrimSpace(string(out))
}
