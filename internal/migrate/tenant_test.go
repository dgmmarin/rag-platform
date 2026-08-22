package migrate

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestTenantsValidatesInputs proves the runner fails fast (before dialing any
// database) on the two misconfigurations that would otherwise surface as opaque
// errors mid-run: an empty control-plane URL and a nil decrypter (SPEC-09 §2:
// passwords are always envelope-encrypted, so a decrypter is mandatory).
func TestTenantsValidatesInputs(t *testing.T) {
	if _, err := Tenants(context.Background(), "", TenantOptions{Decrypter: stubDecrypter{}}); err == nil {
		t.Fatal("Tenants with empty control URL: want error, got nil")
	}
	if _, err := Tenants(context.Background(), "postgres://x/y", TenantOptions{}); err == nil {
		t.Fatal("Tenants with nil decrypter: want error, got nil")
	}
}

// stubDecrypter is a no-op Decrypter for the input-validation test (never used to
// dial a database).
type stubDecrypter struct{}

func (stubDecrypter) Decrypt(b []byte) ([]byte, error) { return b, nil }

// TestTenantResultHasFailures covers the summary predicate the CLI uses to set a
// non-zero exit code (SPEC-01 §7).
func TestTenantResultHasFailures(t *testing.T) {
	var empty TenantResult
	if empty.HasFailures() {
		t.Fatal("empty result should have no failures")
	}
	withFail := TenantResult{Failed: []TenantOutcome{{Slug: "x", Err: errors.New("boom")}}}
	if !withFail.HasFailures() {
		t.Fatal("result with a failed tenant must report HasFailures")
	}
}

// TestMemFSEnumerable proves the in-memory FS the runner hands goose supports the
// enumeration goose relies on (glob/readdir/open), so a substituted migration set
// is discoverable exactly like the embedded one.
func TestMemFSEnumerable(t *testing.T) {
	sub, err := substituteDimensionFS(tenantMigrations, 1536)
	if err != nil {
		t.Fatalf("substituteDimensionFS: %v", err)
	}
	rooted, err := fs.Sub(sub, "tenant")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	matches, err := fs.Glob(rooted, "*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no migrations visible through the substituted FS")
	}
	if _, err := fs.ReadFile(rooted, matches[0]); err != nil {
		t.Fatalf("read %s through substituted FS: %v", matches[0], err)
	}
}

// TestExpectedTenantVersion pins the expected tenant schema version to the
// highest embedded tenant migration. The resolver fails closed against any
// tenant behind this number (SPEC-01 §7), so it must track the migrations, not a
// hand-maintained constant that can silently drift.
func TestExpectedTenantVersion(t *testing.T) {
	got, err := ExpectedTenantVersion()
	if err != nil {
		t.Fatalf("ExpectedTenantVersion: %v", err)
	}
	// Migration 00001_initial_schema.sql is version 1; bump this alongside a new
	// migration file.
	if got != 1 {
		t.Fatalf("ExpectedTenantVersion = %d, want 1", got)
	}
}

// TestTenantSchemaMatchesMigrations is the tenant-side drift guard, mirroring
// the control-plane one: schemas/tenant.sql must stay equal (modulo whitespace)
// to what the goose tenant migrations apply at the documented default embedding
// dimension, so the schema file never lies about the provisioned shape.
func TestTenantSchemaMatchesMigrations(t *testing.T) {
	schema := repoFile(t, "schemas/tenant.sql")

	migrated, err := TenantSchemaSQL(defaultEmbeddingDim)
	if err != nil {
		t.Fatalf("TenantSchemaSQL: %v", err)
	}

	if got, want := normalizeSQL(migrated), normalizeSQL(schema); got != want {
		t.Fatalf("tenant migrations drifted from schemas/tenant.sql\n"+
			"regenerate one from the other.\n--- migrations ---\n%s\n--- schema ---\n%s", got, want)
	}
}

// TestTenantMigrationHasDimensionPlaceholder proves migration 0001 carries the
// EMBEDDING_DIM placeholder rather than a hard-coded dimension: substitution is
// what lets one migration serve tenants with different embedding models
// (SPEC-01 §6/§7).
func TestTenantMigrationHasDimensionPlaceholder(t *testing.T) {
	entries, err := fs.Glob(tenantMigrations, "tenant/*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	found := false
	for _, name := range entries {
		b, err := tenantMigrations.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), dimensionPlaceholder) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no tenant migration contains the %q placeholder", dimensionPlaceholder)
	}
}

// TestSubstituteDimensionFS renders the placeholder to a concrete vector(N) when
// the runner reads a migration for a given tenant, and rejects an invalid
// dimension (fail closed rather than applying a broken schema).
func TestSubstituteDimensionFS(t *testing.T) {
	sub, err := substituteDimensionFS(tenantMigrations, 768)
	if err != nil {
		t.Fatalf("substituteDimensionFS(768): %v", err)
	}
	b, err := fs.ReadFile(sub, "tenant/00001_initial_schema.sql")
	if err != nil {
		t.Fatalf("read substituted migration: %v", err)
	}
	if strings.Contains(string(b), dimensionPlaceholder) {
		t.Fatalf("placeholder %q not substituted", dimensionPlaceholder)
	}
	if !strings.Contains(string(b), "vector(768)") {
		t.Fatalf("expected vector(768) after substitution, got:\n%s", b)
	}

	for _, bad := range []int{0, -1} {
		if _, err := substituteDimensionFS(tenantMigrations, bad); err == nil {
			t.Fatalf("substituteDimensionFS(%d): want error, got nil", bad)
		}
	}
}
