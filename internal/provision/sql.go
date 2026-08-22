package provision

import (
	"fmt"
	"strings"
)

// requiredExtensions are the extensions the tenant schema (SPEC-03) assumes
// exist. They are superuser-only, so the privileged provisioning connection
// installs them at database-create time; a tenant migration cannot (it runs as
// the least-privilege per-tenant role) — SPEC-01 §6, ADR-0015.
var requiredExtensions = []string{"vector", "pgcrypto", "pg_trgm"}

// extensionStatements returns the idempotent CREATE EXTENSION statements to run
// inside the new tenant database on the privileged connection.
func extensionStatements() []string {
	stmts := make([]string, 0, len(requiredExtensions))
	for _, ext := range requiredExtensions {
		// Extension names are a fixed allowlist above, not user input, so a
		// literal is safe here; kept lowercase to match the schema docs.
		stmts = append(stmts, "create extension if not exists "+ext)
	}
	return stmts
}

// quoteLiteral single-quote-escapes a string for use as an SQL string literal.
// Passwords are generated from a restricted alphabet (idents.go) but escaping is
// applied unconditionally so the builder is safe regardless of the caller.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// createRoleSQL builds a least-privilege CREATE ROLE for the tenant: it can log
// in with the given password but is not a superuser and cannot create further
// databases or roles. The role identifier is validated+quoted (fail closed) and
// the password is single-quote-escaped.
func createRoleSQL(role, password string) (string, error) {
	q, err := quoteIdent(role)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE",
		q, quoteLiteral(password)), nil
}

// createDatabaseSQL builds CREATE DATABASE owned by the tenant role, so the role
// has full rights inside its own database and none outside it (C-1 isolation).
func createDatabaseSQL(dbName, owner string) (string, error) {
	qdb, err := quoteIdent(dbName)
	if err != nil {
		return "", err
	}
	qowner, err := quoteIdent(owner)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("CREATE DATABASE %s OWNER %s", qdb, qowner), nil
}

// lockdownDatabaseSQL builds the statements that close the connection-level
// boundary NFR-SEC-01 mandates. Postgres grants CONNECT on a new database to
// PUBLIC by default, which would let any other tenant's least-privilege role open
// a session against this database (and enumerate its catalog), even though
// table-level ownership keeps the data itself unreadable. Revoking CONNECT from
// PUBLIC and granting it back only to the owning role means no query path from
// another tenant can address this database at all (SPEC-01 §9, ADR-0018).
//
// Both statements are idempotent, so re-running provisioning (retry safety) is a
// no-op. Identifiers are validated+quoted (fail closed); GRANT/REVOKE targets
// cannot be parameterised.
func lockdownDatabaseSQL(dbName, owner string) ([]string, error) {
	qdb, err := quoteIdent(dbName)
	if err != nil {
		return nil, err
	}
	qowner, err := quoteIdent(owner)
	if err != nil {
		return nil, err
	}
	return []string{
		fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", qdb),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", qdb, qowner),
	}, nil
}

// dropDatabaseSQL builds an idempotent DROP DATABASE for tenant teardown after
// the grace period (SPEC-01 §8). WITH (FORCE) terminates any lingering backend
// sessions (a suspended tenant's evicted pool, a stray admin connection) so the
// drop does not fail on "database is being accessed by other users". The name is
// validated+quoted (fail closed); DROP DATABASE cannot be parameterised.
func dropDatabaseSQL(dbName string) (string, error) {
	qdb, err := quoteIdent(dbName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", qdb), nil
}

// dropRoleSQL builds an idempotent DROP ROLE for tenant teardown. It runs after
// the database is dropped, so the role owns nothing and the drop cannot fail on
// dependent objects. The name is validated+quoted (fail closed).
func dropRoleSQL(role string) (string, error) {
	qrole, err := quoteIdent(role)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("DROP ROLE IF EXISTS %s", qrole), nil
}
