//go:build e2e

// STORY-02.5 golden path: tenant move (connection update) against the REAL local
// stack (Postgres+pgvector via `mise run up`), with NO mocks. It provisions a
// tenant with the real `ragctl enroll` on one database/role, then moves it to a
// second database/role with a rotated password via the real `ragctl tenant move`
// command, and asserts through the real resolver and database (FR-TEN-07,
// SPEC-01 §4):
//   - before the move, the resolver's DB handle is connected to the ORIGINAL
//     database (current_database() == D1);
//   - `tenant tenant move` re-encrypts the new password (it round-trips through
//     decrypt back to the plaintext we supplied) and repoints the record;
//   - after the move + Resolver.Close, the next Open builds a NEW pool against
//     the NEW connection (current_database() == D2) — the old pool is gone;
//   - the new least-privilege role can log in with the rotated password (the
//     credentials the move recorded actually work end to end).
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTenantMoveGoldenPath(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t) // wraps migrateDEK; enroll + move + resolver share it

	slug := "move-" + strings.ReplaceAll(mustSuffix(t), "-", "")
	const dim = 768
	suffix := strings.ReplaceAll(mustSuffix(t), "-", "")
	newDB := "tenant_moved_" + suffix
	newRole := "role_moved_" + suffix
	newPassword := "MovedPw_" + suffix
	port := hostPort("POSTGRES_PORT", "5432")

	// Teardown: drop both the original and the move-target database/role, plus the
	// control-plane rows, regardless of where the test fails.
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		origDB := tryScalar(slug, "d.database_name")
		origRole := tryScalar(slug, "d.username")
		for _, db := range []string{origDB, newDB} {
			if db != "" {
				_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", db))
			}
		}
		for _, role := range []string{origRole, newRole} {
			if role != "" {
				_ = tryPsql(user, "control_plane", fmt.Sprintf("DROP ROLE IF EXISTS %s", role))
			}
		}
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE slug = '%s'", slug))
	})

	// --- Provision a real tenant on its original database (D1/R1). ---
	if out, exit := runEnroll(t, ageKey, blob, slug, "Move Co", dim); exit != 0 {
		t.Fatalf("enroll exited %d\n%s", exit, out)
	}
	id := tenantIDForSlug(t, slug)
	origDB := tenantScalar(t, slug, "d.database_name")

	res := lifecycleResolver(t)
	ctx := context.Background()

	// Before the move, the resolver connects to the ORIGINAL database.
	db1, err := res.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open before move: %v", err)
	}
	var connectedBefore string
	if err := db1.QueryRow(ctx, "select current_database()").Scan(&connectedBefore); err != nil {
		t.Fatalf("current_database before move: %v", err)
	}
	if connectedBefore != origDB {
		t.Fatalf("before move connected to %q, want original %q", connectedBefore, origDB)
	}

	// --- Prepare the move target: a second least-privilege role + database. ---
	// This stands in for the operator's out-of-band copy/restore of the tenant
	// database to the new host (the runbook step); the move only repoints the
	// registry (SPEC-01 §4).
	execPsql(t, "control_plane", fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE", newRole, newPassword))
	execPsql(t, "control_plane", fmt.Sprintf("CREATE DATABASE %s OWNER %s", newDB, newRole))

	// --- Move: repoint the connection to D2/R2 with the rotated password. ---
	out, exit := runTenant(t, ageKey, blob, "move",
		"--slug", slug,
		"--db-host", "localhost",
		"--db-port", port,
		"--db-name", newDB,
		"--db-user", newRole,
		"--db-ssl-mode", "disable",
		"--db-password", newPassword,
	)
	if exit != 0 {
		t.Fatalf("tenant move exited %d\n%s", exit, out)
	}

	// The record now points at the new database/role.
	if got := tenantScalar(t, slug, "d.database_name"); got != newDB {
		t.Fatalf("after move database_name = %q, want %q", got, newDB)
	}
	if got := tenantScalar(t, slug, "d.username"); got != newRole {
		t.Fatalf("after move username = %q, want %q", got, newRole)
	}

	// The rotated password round-trips: stored ciphertext decrypts back to the
	// plaintext we supplied (encrypt on write == decrypt on read, SPEC-09 §2).
	encHex := psqlScalar(t, fmt.Sprintf(
		"select encode(d.password_enc, 'hex') from tenant_databases d "+
			"join tenants t on t.id = d.tenant_id where t.slug = '%s'", slug))
	if got := decryptHex(t, encHex); got != newPassword {
		t.Fatalf("stored password did not round-trip after move: got %q, want the supplied password", got)
	}
	// The new role can actually log in with the rotated password.
	execPsqlAs(t, newRole, newPassword, newDB, "select 1")

	// A move audit event was written (C-3: control-plane only, never the password).
	if got := psqlScalar(t, fmt.Sprintf(
		"select count(*) from audit_log a join tenants t on t.id = a.tenant_id "+
			"where t.slug = '%s' and a.action = 'tenant.move'", slug)); got == "0" {
		t.Fatal("no tenant.move audit event written")
	}

	// --- After the move + Close, the next Open builds a NEW pool against D2. ---
	res.Close(id)
	time.Sleep(80 * time.Millisecond)
	db2, err := res.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after move: %v", err)
	}
	var connectedAfter string
	if err := db2.QueryRow(ctx, "select current_database()").Scan(&connectedAfter); err != nil {
		t.Fatalf("current_database after move: %v", err)
	}
	if connectedAfter != newDB {
		t.Fatalf("after move connected to %q, want new %q (pool not rebuilt against new connection)", connectedAfter, newDB)
	}
	if connectedAfter == connectedBefore {
		t.Fatal("resolver still connected to the original database after the move (old pool not evicted)")
	}
}
