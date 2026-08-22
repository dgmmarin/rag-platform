//go:build e2e

// STORY-03.4 golden path: control-plane API keys against the REAL control-plane
// Postgres (up via `mise run up`), no mocks. It drives the real
// auth.APIKeyService and auth.APIKeyVerifier through:
//   - create (full secret returned once; only a sha256 hash + clear prefix
//     persisted — the plaintext is never stored and cannot be retrieved later),
//   - authenticate a request with the key via the RequireScope middleware
//     (resolves the tenant, allows an in-scope route, and stamps last_used_at),
//   - list (shows prefix, scopes, and last-used; never the secret),
//   - revoke (takes effect immediately: the next request is rejected),
//   - an EXPIRED key is rejected,
//   - a request outside the key's scope is refused (403).
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

func TestAPIKeyLifecycleGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)
	svc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "apikey-" + suffix

	// --- Seed a tenant to own the keys. ---
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		slug, "API Key Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	// --- Create: the full secret is returned ONCE. ---
	rec, secret, err := svc.Create(ctx, auth.CreateKeyParams{
		TenantID: tenantID, Name: "ci-token", Scopes: []string{"query", "ingest"},
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if secret == "" || secret[:3] != "rk_" {
		t.Fatalf("create returned secret %q, want an rk_ plaintext key", secret)
	}

	// The plaintext is NOT stored: the row holds only a hash + a clear prefix,
	// and no column equals the secret.
	storedPrefix := psqlScalar(t, fmt.Sprintf(
		"select key_prefix from api_keys where id = '%s'", rec.ID))
	if storedPrefix != rec.Prefix {
		t.Fatalf("stored prefix %q != returned prefix %q", storedPrefix, rec.Prefix)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from api_keys where id = '%s' and encode(key_hash,'hex') = '%s'",
		rec.ID, secret)); n != "0" {
		t.Fatal("key_hash stores the plaintext secret; want a hash only")
	}
	// key_hash is present and is the sha256 (32 bytes) of the secret.
	if hlen := psqlScalar(t, fmt.Sprintf(
		"select octet_length(key_hash) from api_keys where id = '%s'", rec.ID)); hlen != "32" {
		t.Fatalf("key_hash length = %q bytes, want 32 (sha256)", hlen)
	}

	// --- Authenticate a request with the key through RequireScope(query). ---
	var resolvedTenant string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := tenant.TenantIDFromCtx(r.Context()); ok {
			resolvedTenant = id.String()
		}
		w.WriteHeader(http.StatusOK)
	})
	authed := verifier.RequireScope(auth.ScopeQuery)(inner)

	doReq := func(h http.Handler, bearer string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := doReq(authed, secret); code != http.StatusOK {
		t.Fatalf("authenticated query: got %d, want 200", code)
	}
	if resolvedTenant != tenantID {
		t.Fatalf("resolved tenant = %q, want %q (derived from the key, FR-ACC-03)", resolvedTenant, tenantID)
	}
	// last_used_at was stamped (SPEC-09 §3).
	if lu := psqlScalar(t, fmt.Sprintf(
		"select last_used_at is not null from api_keys where id = '%s'", rec.ID)); lu != "t" {
		t.Fatal("last_used_at not stamped after an authenticated request")
	}

	// --- List: shows prefix, scopes, and last-used; never the secret. ---
	keys, err := svc.List(ctx, tenantID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("listed %d keys, want 1", len(keys))
	}
	lk := keys[0]
	if lk.Prefix != rec.Prefix {
		t.Fatalf("listed prefix %q != %q", lk.Prefix, rec.Prefix)
	}
	if len(lk.Scopes) != 2 {
		t.Fatalf("listed scopes %v, want 2", lk.Scopes)
	}
	if lk.LastUsedAt == nil {
		t.Fatal("listed key has no last-used timestamp after use")
	}

	// --- Out-of-scope: the query-scoped key is refused an admin route. ---
	adminGuard := verifier.RequireScope(auth.ScopeAdmin)(inner)
	if code := doReq(adminGuard, secret); code != http.StatusForbidden {
		t.Fatalf("out-of-scope request: got %d, want 403", code)
	}

	// --- Revoke: takes effect immediately. ---
	if err := svc.Revoke(ctx, tenantID, rec.ID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if code := doReq(authed, secret); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still authenticates: got %d, want 401", code)
	}
	// Revoke is idempotent.
	if err := svc.Revoke(ctx, tenantID, rec.ID); err != nil {
		t.Fatalf("second revoke should be a no-op: %v", err)
	}

	// --- Expiry honoured: a key that expired in the past is rejected. ---
	past := time.Now().Add(-time.Hour)
	expRec, expSecret, err := svc.Create(ctx, auth.CreateKeyParams{
		TenantID: tenantID, Name: "expired", Scopes: []string{"query"}, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("create expired key: %v", err)
	}
	if code := doReq(authed, expSecret); code != http.StatusUnauthorized {
		t.Fatalf("expired key authenticates: got %d, want 401", code)
	}
	// The verifier reports the distinct expiry reason internally.
	if _, err := verifier.Verify(ctx, expSecret); err != auth.ErrKeyExpired {
		t.Fatalf("Verify(expired) = %v, want ErrKeyExpired", err)
	}
	_ = expRec
}
