package provision

import (
	"strings"
	"testing"
)

// TestExtensionStatements proves provisioning installs exactly the three
// extensions the tenant schema assumes (vector, pgcrypto, pg_trgm), each as an
// idempotent CREATE EXTENSION IF NOT EXISTS run by the privileged connection
// (SPEC-01 §6, ADR-0015). These are superuser-only and must NOT be in a tenant
// migration.
func TestExtensionStatements(t *testing.T) {
	stmts := extensionStatements()
	joined := strings.Join(stmts, "\n")
	for _, ext := range []string{"vector", "pgcrypto", "pg_trgm"} {
		want := "create extension if not exists " + ext
		if !strings.Contains(strings.ToLower(joined), want) {
			t.Errorf("extension statements missing %q\n%s", want, joined)
		}
	}
}

// TestCreateRoleSQLQuotesAndEscapes proves the CREATE ROLE statement quotes the
// role identifier and single-quote-escapes the password literal (a password can
// legitimately contain characters that must be escaped in an SQL string).
func TestCreateRoleSQLQuotesAndEscapes(t *testing.T) {
	sql, err := createRoleSQL("role_acme_ab12cd34", "pa's's")
	if err != nil {
		t.Fatalf("createRoleSQL: %v", err)
	}
	if !strings.Contains(sql, `"role_acme_ab12cd34"`) {
		t.Errorf("role identifier not quoted: %s", sql)
	}
	// Each embedded single quote must be doubled in the SQL literal.
	if !strings.Contains(sql, "'pa''s''s'") {
		t.Errorf("password not single-quote-escaped: %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "LOGIN") {
		t.Errorf("role must be able to LOGIN: %s", sql)
	}
}

// TestCreateRoleSQLRejectsUnsafeRole proves an unsafe role name is rejected
// before any SQL is built (fail closed).
func TestCreateRoleSQLRejectsUnsafeRole(t *testing.T) {
	if _, err := createRoleSQL("role; drop database control_plane", "pw"); err == nil {
		t.Fatal("createRoleSQL accepted an unsafe role name")
	}
}

// TestCreateDatabaseSQLQuotesBoth proves CREATE DATABASE quotes both the
// database and the owning role identifiers.
func TestCreateDatabaseSQLQuotesBoth(t *testing.T) {
	sql, err := createDatabaseSQL("tenant_acme_ab12cd34", "role_acme_ab12cd34")
	if err != nil {
		t.Fatalf("createDatabaseSQL: %v", err)
	}
	if !strings.Contains(sql, `"tenant_acme_ab12cd34"`) || !strings.Contains(sql, `"role_acme_ab12cd34"`) {
		t.Errorf("database/owner not both quoted: %s", sql)
	}
}
