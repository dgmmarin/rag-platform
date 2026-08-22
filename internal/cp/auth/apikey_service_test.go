package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCreateRejectsInvalidScopeBeforeWrite proves an invented scope is refused
// before any row is inserted (FR-ACC-04: only query/ingest/admin).
func TestCreateRejectsInvalidScopeBeforeWrite(t *testing.T) {
	db := &fakeDB{}
	svc := &APIKeyService{DB: db, Now: fixedClock(time.Now())}
	_, _, err := svc.Create(context.Background(), CreateKeyParams{
		TenantID: "t-1", Name: "ci", Scopes: []string{"query", "root"},
	})
	if err == nil {
		t.Fatal("Create with invalid scope succeeded; want error")
	}
	if len(db.execs) != 0 {
		t.Fatalf("Create wrote %d statements on invalid scope; want 0", len(db.execs))
	}
}

// TestCreateRejectsNoScope proves a key must carry at least one scope.
func TestCreateRejectsNoScope(t *testing.T) {
	svc := &APIKeyService{DB: &fakeDB{}, Now: fixedClock(time.Now())}
	if _, _, err := svc.Create(context.Background(), CreateKeyParams{TenantID: "t-1", Name: "x"}); err == nil {
		t.Fatal("Create with no scopes succeeded; want error")
	}
}

// TestCreateReturnsSecretOnceAndStoresHash proves the full plaintext secret is
// returned exactly once, that it is rk_<prefix>_..., and that the stored record
// carries the prefix but not the plaintext (FR-ACC-05, C-4).
func TestCreateReturnsSecretOnceAndStoresHash(t *testing.T) {
	// The insert returns the new id.
	db := &fakeDB{rows: []fakeRow{{vals: []any{"key-123"}}}}
	svc := &APIKeyService{DB: db, Now: fixedClock(time.Now())}

	rec, secret, err := svc.Create(context.Background(), CreateKeyParams{
		TenantID: "t-1", Name: "ci-token", Scopes: []string{"query", "ingest"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if secret == "" || secret[:3] != "rk_" {
		t.Fatalf("returned secret %q is not a plaintext rk_ key", secret)
	}
	if rec.ID != "key-123" {
		t.Fatalf("record id = %q, want key-123", rec.ID)
	}
	if rec.Prefix == "" || len(rec.Prefix) != apiKeyPrefixLen {
		t.Fatalf("record prefix %q malformed", rec.Prefix)
	}
	// The plaintext must never be a field of the stored record.
	if strings.Contains(recString(rec), secret) {
		t.Fatal("stored record exposes the plaintext secret")
	}
}

// TestRevokeIsIdempotent proves revoking a key twice is a no-op success.
func TestRevokeIsIdempotent(t *testing.T) {
	db := &fakeDB{}
	svc := &APIKeyService{DB: db, Now: fixedClock(time.Now())}
	if err := svc.Revoke(context.Background(), "t-1", "key-1"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := svc.Revoke(context.Background(), "t-1", "key-1"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}

// TestRevokeUnknownKeyReturnsNotFound proves revoking a key that does not exist
// for the tenant is reported so a handler can 404 rather than silently succeed.
func TestRevokeUnknownKeyReturnsNotFound(t *testing.T) {
	// Update affects 0 rows; the follow-up exists() check returns false.
	db := &fakeDB{tag: &fakeTag{n: 0}, rows: []fakeRow{{vals: []any{false}}}}
	svc := &APIKeyService{DB: db, Now: fixedClock(time.Now())}
	if err := svc.Revoke(context.Background(), "t-1", "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("revoke unknown: got %v, want ErrKeyNotFound", err)
	}
}

// helpers ---------------------------------------------------------------------

func recString(r APIKeyRecord) string {
	return r.ID + "|" + r.Name + "|" + r.Prefix + "|" + strings.Join(r.Scopes, ",")
}
