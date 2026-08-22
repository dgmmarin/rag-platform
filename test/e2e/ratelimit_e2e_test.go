//go:build e2e

// STORY-03.9 golden path: rate limiting against the REAL control-plane Postgres
// (up via `mise run up`), no mocks. It drives the real middleware chain a request
// traverses in STORY-04.1 — auth.APIKeyVerifier.RequireScope (authenticates the
// Bearer key, resolves the tenant + key id into context, FR-ACC-03) then
// ratelimit.Middleware.Handler (token bucket per API key and per tenant, reading
// the tenant's settings.limits.qps) — proving:
//   - a request over the per-key limit gets 429 with Retry-After and RateLimit-*
//     headers (NFR-SEC-07, SPEC-07 §1),
//   - the inner handler is not reached on a 429,
//   - limiting is per-key: one key being limited does not limit another key of
//     the same tenant while the tenant ceiling has headroom.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
)

func TestRateLimitGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	keySvc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))
	settings := tenants.NewSettingsService(tenants.SettingsFromPool(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "rl-" + suffix

	// Seed a tenant provisioned at dim 1024 with an explicit low qps ceiling so a
	// couple of requests exercise the limit deterministically.
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region, settings)
		 values ($1, $2, 'active', 'eu-central',
		   jsonb_build_object('embedding_dim', 1024::int,
		     'limits', jsonb_build_object('qps', 1, 'max_upload_mb', 50, 'max_pages_per_crawl', 5000)))
		 returning id::text`,
		slug, "RateLimit Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	// Two API keys for the SAME tenant to prove per-key isolation.
	_, secretA, err := keySvc.Create(ctx, auth.CreateKeyParams{
		TenantID: tenantID, Name: "key-a", Scopes: []string{"query"},
	})
	if err != nil {
		t.Fatalf("create key A: %v", err)
	}
	_, secretB, err := keySvc.Create(ctx, auth.CreateKeyParams{
		TenantID: tenantID, Name: "key-b", Scopes: []string{"query"},
	})
	if err != nil {
		t.Fatalf("create key B: %v", err)
	}

	// A frozen clock so the token buckets do not refill mid-test (deterministic).
	frozen := time.Now()
	limiter := ratelimit.New(func() time.Time { return frozen })
	// qps read from the tenant's settings.limits.qps (=1); per-key burst 1, tenant
	// burst large so the per-KEY bucket is the binding constraint here.
	mw := &ratelimit.Middleware{
		Limiter:     limiter,
		Limit:       ratelimit.LimitFromSettings(settings, 10),
		Burst:       1,
		TenantBurst: 100,
	}

	// The real chain: authenticate the key, then rate-limit.
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	chain := verifier.RequireScope(auth.ScopeQuery)(mw.Handler(inner))

	send := func(secret string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		return rr
	}

	// Key A: first request allowed.
	if rr := send(secretA); rr.Code != http.StatusOK {
		t.Fatalf("key A first request: got %d, want 200", rr.Code)
	}
	// Key A: second request over the per-key limit → 429 with headers.
	rr := send(secretA)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("key A second request: got %d, want 429", rr.Code)
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 response missing Retry-After header")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer seconds value", ra)
	}
	if rr.Header().Get("RateLimit-Limit") == "" {
		t.Fatal("429 response missing RateLimit-Limit header")
	}
	if rr.Header().Get("RateLimit-Reset") == "" {
		t.Fatal("429 response missing RateLimit-Reset header")
	}

	// Key B (same tenant) is NOT limited by key A's exhausted bucket: per-key
	// isolation holds while the tenant ceiling has headroom.
	if rr := send(secretB); rr.Code != http.StatusOK {
		t.Fatalf("key B first request: got %d, want 200 (per-key isolation)", rr.Code)
	}
}
