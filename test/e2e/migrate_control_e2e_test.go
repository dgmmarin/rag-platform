//go:build e2e

// STORY-01.5 golden path: `ragctl migrate control` applies the embedded goose
// migrations to the real control-plane Postgres (up via `mise run up`), creating
// the documented schema. Asserted against the real database, no mocks: key
// tables and the required extensions must exist, and a second run is a no-op
// (goose idempotency).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// controlPlaneURL returns the URL the ragctl binary (a host process) uses to
// reach the control-plane database, honouring CONTROL_PLANE_URL if set and
// otherwise reconstructing it from the POSTGRES_* compose vars the same way the
// stack does.
func controlPlaneURL() string {
	if v := os.Getenv("CONTROL_PLANE_URL"); v != "" {
		return v
	}
	user := hostPort("POSTGRES_USER", "rag")
	pass := hostPort("POSTGRES_PASSWORD", "rag")
	db := hostPort("POSTGRES_DB", "control_plane")
	port := hostPort("POSTGRES_PORT", "5432")
	return fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable", user, pass, port, db)
}

// psqlScalar runs a single-value query inside the postgres container and returns
// the trimmed result.
func psqlScalar(t *testing.T, query string) string {
	t.Helper()
	user := hostPort("POSTGRES_USER", "rag")
	db := hostPort("POSTGRES_DB", "control_plane")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", user, "-d", db, "-tAc", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q failed: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMigrateControlAppliesSchema(t *testing.T) {
	bin := buildRagctl(t)
	url := controlPlaneURL()

	runMigrate := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "migrate", "control")
		cmd.Env = append(withoutEnv(os.Environ(), "CONTROL_PLANE_URL"), "CONTROL_PLANE_URL="+url)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ragctl migrate control: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "applied control-plane migrations") {
			t.Fatalf("migrate control output %q missing success line", out)
		}
	}

	// First application against a fresh (or already-migrated) database.
	runMigrate()

	// Key tables the control plane depends on must exist (SPEC-03, SPEC-08).
	for _, table := range []string{"tenants", "tenant_databases", "jobs"} {
		got := psqlScalar(t, fmt.Sprintf(
			"select to_regclass('public.%s') is not null", table))
		if got != "t" {
			t.Fatalf("table %q missing after migrate control (got %q)", table, got)
		}
	}

	// Both extensions must be installed — citext is the latent-bug guard: the
	// schema uses citext for users.email, so a migration that forgets to create
	// it would have failed the runMigrate above on a fresh DB.
	for _, ext := range []string{"pgcrypto", "citext"} {
		got := psqlScalar(t, fmt.Sprintf(
			"select count(*) from pg_extension where extname = '%s'", ext))
		if got != "1" {
			t.Fatalf("extension %q not installed after migrate control (count=%q)", ext, got)
		}
	}

	// Second run must be a no-op: goose records applied versions, so rerunning
	// succeeds without error and leaves the version unchanged.
	before := psqlScalar(t, "select max(version_id) from goose_db_version")
	runMigrate()
	after := psqlScalar(t, "select max(version_id) from goose_db_version")
	if before != after {
		t.Fatalf("migrate control not idempotent: version %q -> %q", before, after)
	}
}
