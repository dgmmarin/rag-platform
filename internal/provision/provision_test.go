package provision

import (
	"strings"
	"testing"
)

// TestQuoteIdentRejectsUnsafe proves the identifier quoter fails closed on
// anything that is not a plain lowercase Postgres identifier. Role/database
// names flow into DDL that cannot be parameterised (CREATE ROLE/DATABASE), so an
// unsafe name must be rejected, not escaped, to keep provisioning injection-proof
// (SPEC-09).
func TestQuoteIdentRejectsUnsafe(t *testing.T) {
	bad := []string{
		"",                      // empty
		"role; drop table",      // injection attempt
		"role name",             // whitespace
		"role\"quote",           // embedded quote
		"Role",                  // uppercase (names are derived lowercase)
		"role-dash",             // dash is not allowed in our derived names
		"1role",                 // must not start with a digit
		strings.Repeat("a", 64), // over the 63-byte Postgres limit
	}
	for _, name := range bad {
		if _, err := quoteIdent(name); err == nil {
			t.Errorf("quoteIdent(%q) = nil error, want rejection", name)
		}
	}
}

// TestQuoteIdentAcceptsDerivedNames proves the names provisioning derives are
// accepted and wrapped in double quotes.
func TestQuoteIdentAcceptsDerivedNames(t *testing.T) {
	good := []string{"tenant_acme_ab12cd34", "role_acme_ab12cd34", "control_plane"}
	for _, name := range good {
		got, err := quoteIdent(name)
		if err != nil {
			t.Fatalf("quoteIdent(%q) unexpected error: %v", name, err)
		}
		if got != `"`+name+`"` {
			t.Errorf("quoteIdent(%q) = %q, want %q", name, got, `"`+name+`"`)
		}
	}
}

// TestGeneratePasswordIsRandomAndStrong proves generated passwords are
// non-empty, unique across calls, and printable ASCII (safe to place in a DSN
// and a CREATE ROLE ... PASSWORD literal).
func TestGeneratePasswordIsRandomAndStrong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		pw, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword: %v", err)
		}
		if len(pw) < 24 {
			t.Fatalf("password too short (%d chars): %q", len(pw), pw)
		}
		if seen[pw] {
			t.Fatalf("generatePassword returned a duplicate: %q", pw)
		}
		seen[pw] = true
		for _, r := range pw {
			if r < '!' || r > '~' || r == '\'' || r == '"' || r == '\\' {
				t.Fatalf("password contains unsafe char %q in %q", r, pw)
			}
		}
	}
}

// TestDeriveNamesFromSlug proves role/database names are derived deterministically
// from the tenant slug plus a short unique suffix, sanitised to a safe form.
func TestDeriveNamesFromSlug(t *testing.T) {
	dbName, role := deriveNames("Acme-Corp!", "ab12cd34")
	if dbName != "tenant_acme_corp_ab12cd34" {
		t.Errorf("dbName = %q", dbName)
	}
	if role != "role_acme_corp_ab12cd34" {
		t.Errorf("role = %q", role)
	}
	// Both derived names must survive the identifier quoter.
	for _, n := range []string{dbName, role} {
		if _, err := quoteIdent(n); err != nil {
			t.Errorf("derived name %q failed quoteIdent: %v", n, err)
		}
	}
}
