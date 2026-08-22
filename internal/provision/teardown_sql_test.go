package provision

import (
	"strings"
	"testing"
)

// TestDropDatabaseSQLQuotesAndForces proves the DROP DATABASE statement quotes
// the database identifier and uses WITH (FORCE) so lingering connections do not
// block the teardown (SPEC-01 §8: after grace, the database is dropped).
func TestDropDatabaseSQLQuotesAndForces(t *testing.T) {
	sql, err := dropDatabaseSQL("tenant_acme_ab12cd34")
	if err != nil {
		t.Fatalf("dropDatabaseSQL: %v", err)
	}
	if !strings.Contains(sql, `"tenant_acme_ab12cd34"`) {
		t.Errorf("database identifier not quoted: %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "IF EXISTS") {
		t.Errorf("drop must be idempotent (IF EXISTS): %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "FORCE") {
		t.Errorf("drop must FORCE-terminate connections: %s", sql)
	}
}

// TestDropDatabaseSQLRejectsUnsafeName proves an unsafe database name is rejected
// before any SQL is built (fail closed), mirroring the create side.
func TestDropDatabaseSQLRejectsUnsafeName(t *testing.T) {
	if _, err := dropDatabaseSQL("x; drop database control_plane"); err == nil {
		t.Fatal("dropDatabaseSQL accepted an unsafe database name")
	}
}

// TestDropRoleSQLQuotesAndIsIdempotent proves DROP ROLE quotes the identifier and
// is idempotent (IF EXISTS), so a re-run after a partial teardown is safe.
func TestDropRoleSQLQuotesAndIsIdempotent(t *testing.T) {
	sql, err := dropRoleSQL("role_acme_ab12cd34")
	if err != nil {
		t.Fatalf("dropRoleSQL: %v", err)
	}
	if !strings.Contains(sql, `"role_acme_ab12cd34"`) {
		t.Errorf("role identifier not quoted: %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "IF EXISTS") {
		t.Errorf("drop role must be idempotent (IF EXISTS): %s", sql)
	}
}

// TestDropRoleSQLRejectsUnsafeName proves an unsafe role name is rejected.
func TestDropRoleSQLRejectsUnsafeName(t *testing.T) {
	if _, err := dropRoleSQL("role; drop database control_plane"); err == nil {
		t.Fatal("dropRoleSQL accepted an unsafe role name")
	}
}
