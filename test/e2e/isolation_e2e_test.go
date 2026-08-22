//go:build e2e

// STORY-02.6 — tenant isolation test suite (NFR-SEC-01, SPEC-01 §9).
//
// Two tenants (A and B) are enrolled on the REAL local stack via `ragctl enroll`
// (no mocks). Each gets a least-privilege role and a dedicated database (C-1,
// ADR-0001). The suite then proves, at the layer that exists today (the resolver
// + tenant.DB — the ONLY path to tenant data, ADR-0003), that there is zero
// cross-tenant leakage:
//
//   - Resolving with A's ID yields a handle bound to A's database, never B's, and
//     vice versa (FR-ACC-03: identity comes from the principal/registry, never a
//     client parameter).
//   - Data written under B is invisible through A's resolved connection: A's role
//     cannot even name B's database, so a cross-database read is impossible.
//   - A's credentials presented against B's database are rejected by Postgres
//     (isolation enforced at the connection level — NFR-SEC-01).
//
// The cross-tenant HTTP endpoint matrix (A's credentials against B's IDs over
// every tenant-scoped route, asserting 404/403) is deferred to EPIC-04 /
// STORY-04.1, when the public router exists. openTenantByID/isolationTenant here
// give that follow-up its two-tenant fixture; only the HTTP driver is missing.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// isolationTenant is one enrolled tenant plus the facts the isolation assertions
// need: its registry ID, its dedicated database/role, and a distinctive secret
// row planted in its own database.
type isolationTenant struct {
	slug     string
	id       tenant.ID
	dbName   string
	role     string
	password string
	secret   string // unique marker row written into this tenant's DB only
}

// isolationResolver builds a resolver whose Decrypter matches the DEK that
// `ragctl enroll` used (migrateDEK), so it can decrypt the enrolled tenants'
// stored passwords and build their pools (SPEC-09 §2).
func isolationResolver(t *testing.T) tenant.Resolver {
	t.Helper()
	cipher, err := crypto.NewCipher(1, migrateDEK)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return tenant.NewResolver(tenant.Config{
		ControlPool: controlPool(t),
		Decrypter:   cipher,
		CacheTTL:    50 * time.Millisecond,
	})
}

// enrollIsolationTenant enrols one tenant end to end and plants a unique secret
// row in its own database (written as the tenant's own least-privilege role).
func enrollIsolationTenant(t *testing.T, ageKey, blob, label string) isolationTenant {
	t.Helper()
	slug := "iso-" + label + "-" + strings.ReplaceAll(mustSuffix(t), "-", "")

	t.Cleanup(func() {
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

	out, exit := runEnroll(t, ageKey, blob, slug, "Isolation "+label, 768)
	if exit != 0 {
		t.Fatalf("enroll %s exited %d\n%s", slug, exit, out)
	}

	idStr := tenantScalar(t, slug, "t.id")
	id, err := parseTenantID(idStr)
	if err != nil {
		t.Fatalf("parse tenant id %q: %v", idStr, err)
	}
	dbName := tenantScalar(t, slug, "d.database_name")
	role := tenantScalar(t, slug, "d.username")
	encHex := psqlScalar(t, fmt.Sprintf(
		"select encode(d.password_enc, 'hex') from tenant_databases d "+
			"join tenants t on t.id = d.tenant_id where t.slug = '%s'", slug))
	password := decryptHex(t, encHex)

	// Plant a distinctive secret in this tenant's own DB, written as the tenant's
	// own role (proving the role owns and can write its database). The chunks
	// table exists from the tenant migrations; a bespoke marker table keeps the
	// assertion self-contained and unambiguous per tenant.
	secret := "secret-" + label + "-" + strings.ReplaceAll(mustSuffix(t), "-", "")
	execPsqlAs(t, role, password, dbName,
		fmt.Sprintf("CREATE TABLE iso_marker (v text); INSERT INTO iso_marker VALUES ('%s')", secret))

	return isolationTenant{
		slug: slug, id: id, dbName: dbName, role: role, password: password, secret: secret,
	}
}

// TestTenantIsolationSuite is the STORY-02.6 golden path: two tenants, zero
// cross-tenant leakage at the resolver/DB layer.
func TestTenantIsolationSuite(t *testing.T) {
	migrateControl(t)
	ageKey, blob := writeWrappedDEK(t)

	a := enrollIsolationTenant(t, ageKey, blob, "a")
	b := enrollIsolationTenant(t, ageKey, blob, "b")

	// Sanity: the two tenants really are separate databases and roles (C-1).
	if a.dbName == b.dbName || a.role == b.role || a.id == b.id {
		t.Fatalf("tenants A and B are not distinct: A=%+v B=%+v", a, b)
	}

	res := isolationResolver(t)
	ctx := context.Background()

	adb, err := res.Open(ctx, a.id)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	bdb, err := res.Open(ctx, b.id)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	// (1) Resolving A's ID yields A's secret and only A's; resolving B's ID yields
	// B's. This proves the resolver binds identity -> the correct database and
	// never crosses over (FR-ACC-03).
	if got := readMarker(ctx, t, adb); got != a.secret {
		t.Fatalf("A's handle read %q, want A's secret %q", got, a.secret)
	}
	if got := readMarker(ctx, t, bdb); got != b.secret {
		t.Fatalf("B's handle read %q, want B's secret %q", got, b.secret)
	}
	if readMarker(ctx, t, adb) == b.secret {
		t.Fatal("A's handle leaked B's secret")
	}

	// (2) A's resolved connection cannot address B's database at all. A cross-
	// database reference (dbname.schema.table is not even valid in Postgres) or an
	// attempt to read B's marker returns nothing/errors — never B's secret. The
	// only table A can see named iso_marker is A's own.
	var leaked string
	err = adb.QueryRow(ctx, "SELECT v FROM iso_marker WHERE v = $1", b.secret).Scan(&leaked)
	if err == nil && leaked == b.secret {
		t.Fatalf("A's connection read B's secret %q — cross-tenant leak", b.secret)
	}

	// (3) Isolation is enforced at the connection level (NFR-SEC-01): A's
	// credentials presented against B's database are rejected by Postgres. This is
	// the last line of defence if identity resolution were ever bypassed.
	if err := connectAsRoleToDB(ctx, a.role, a.password, b.dbName); err == nil {
		t.Fatalf("A's role %q authenticated into B's database %q — isolation breach", a.role, b.dbName)
	}
	// Control: A's own credentials against A's own database DO work, proving the
	// rejection above is about cross-tenant access, not a broken credential.
	if err := connectAsRoleToDB(ctx, a.role, a.password, a.dbName); err != nil {
		t.Fatalf("A's role cannot reach its OWN database %q: %v", a.dbName, err)
	}

	// (4) Guessing IDs does not help: opening B's ID never yields A's pool/data.
	// (Handles are bound at Open; there is no client-supplied tenant parameter to
	// tamper with — FR-ACC-03. The HTTP endpoint matrix that exercises guessed IDs
	// over the wire is the EPIC-04 follow-up documented in this file's header.)
	if readMarker(ctx, t, bdb) == a.secret {
		t.Fatal("B's handle leaked A's secret when opened by ID")
	}
}

// readMarker reads the single iso_marker value visible through a tenant handle.
func readMarker(ctx context.Context, t *testing.T, db *tenant.DB) string {
	t.Helper()
	var v string
	if err := db.QueryRow(ctx, "SELECT v FROM iso_marker").Scan(&v); err != nil {
		t.Fatalf("read iso_marker via tenant %s: %v", db.ID(), err)
	}
	return v
}

// connectAsRoleToDB attempts a real login as role/password against the named
// database on the host-published Postgres port, returning the connection error
// (nil on success). Used to prove Postgres rejects cross-tenant credentials.
func connectAsRoleToDB(ctx context.Context, role, password, dbName string) error {
	port := hostPort("POSTGRES_PORT", "5432")
	url := fmt.Sprintf("postgres://%s:%s@localhost:%s/%s?sslmode=disable",
		role, password, port, dbName)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	// pgxpool dials lazily; force a real connection + query to surface auth/access
	// errors.
	var one int
	return pool.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// parseTenantID parses the control-plane UUID string into a tenant.ID.
func parseTenantID(s string) (tenant.ID, error) {
	u, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return tenant.ID{}, err
	}
	return tenant.ID(u), nil
}
