//go:build e2e

// Shared psql/ragctl helpers for e2e tests that need to run SQL inside the real
// Postgres container or apply the control-plane migrations. Introduced with
// STORY-02.1 so tenant-resolver tests can create per-tenant roles/databases and
// execute SQL as those roles against the real stack.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// runPsql runs a statement as the given superuser role against database and
// fails the test on error.
func runPsql(t *testing.T, user, database, stmt string) {
	t.Helper()
	if out, err := psql(user, database, stmt); err != nil {
		t.Fatalf("psql (%s/%s) %q failed: %v\n%s", user, database, stmt, err, out)
	}
}

// tryPsql runs a statement and returns the error (used in cleanup where a
// failure should not fail the test).
func tryPsql(user, database, stmt string) error {
	_, err := psql(user, database, stmt)
	return err
}

// psql executes a statement inside the postgres container as user against
// database, returning combined output.
func psql(user, database, stmt string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "postgres",
		"psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", database, "-c", stmt)
	return cmd.CombinedOutput()
}

// execPsqlAs runs a statement as a per-tenant role, authenticating with its
// password over TCP inside the container (proving the least-privilege role can
// operate its own database).
func execPsqlAs(t *testing.T, role, password, database, stmt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T",
		"-e", "PGPASSWORD="+password, "postgres",
		"psql", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-U", role, "-d", database, "-c", stmt)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("psql as %s (%s) %q failed: %v\n%s", role, database, stmt, err, out)
	}
}

// migrateControl applies the control-plane migrations via the real ragctl binary
// (idempotent), so tests that need the control-plane schema are self-contained.
func migrateControl(t *testing.T) {
	t.Helper()
	bin := buildRagctl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "migrate", "control")
	cmd.Env = append(withoutEnv(os.Environ(), "CONTROL_PLANE_URL"), "CONTROL_PLANE_URL="+controlPlaneURL())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ragctl migrate control: %v\n%s", err, out)
	}
}
