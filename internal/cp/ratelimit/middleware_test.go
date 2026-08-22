package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// staticLimit returns a fixed qps for every tenant.
func staticLimit(qps int) LimitFunc {
	return func(context.Context, string) (int, error) { return qps, nil }
}

// reqCtx builds a request whose context carries a resolved tenant and API key id,
// as the auth middleware upstream would.
func reqCtx(tenantID, keyID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	ctx := tenant.WithTenantID(req.Context(), tenant.ID(uuid.MustParse(tenantID)))
	ctx = auth.WithKeyID(ctx, keyID)
	return req.WithContext(ctx)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// Over the limit, the middleware returns 429 with Retry-After and RateLimit-*
// headers and does not call the inner handler.
func TestMiddleware429WithHeaders(t *testing.T) {
	now := time.Unix(0, 0)
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 1}

	called := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(http.StatusOK) })
	h := mw.Handler(inner)

	tid := uuid.NewString()
	do := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqCtx(tid, "key-1"))
		return rr
	}

	if rr := do(); rr.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rr.Code)
	}
	rr := do()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rr.Code)
	}
	if called != 1 {
		t.Fatalf("inner handler called %d times, want 1 (429 must not reach it)", called)
	}
	if ra := rr.Header().Get("Retry-After"); ra != "1" {
		t.Fatalf("Retry-After = %q, want 1", ra)
	}
	if lim := rr.Header().Get("RateLimit-Limit"); lim != "1" {
		t.Fatalf("RateLimit-Limit = %q, want 1", lim)
	}
	if rem := rr.Header().Get("RateLimit-Remaining"); rem != "0" {
		t.Fatalf("RateLimit-Remaining = %q, want 0", rem)
	}
	if _, err := strconv.Atoi(rr.Header().Get("RateLimit-Reset")); err != nil {
		t.Fatalf("RateLimit-Reset = %q, want an integer seconds value", rr.Header().Get("RateLimit-Reset"))
	}
}

// Two different API keys under the same tenant are limited independently as long
// as the shared per-tenant ceiling is not hit (per-key isolation, NFR-SEC-07).
func TestMiddlewarePerKeyIsolation(t *testing.T) {
	now := time.Unix(0, 0)
	// Loose tenant ceiling so only the per-key bucket bites; per-key burst=1.
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 1, TenantBurst: 100}
	h := mw.Handler(okHandler())

	tid := uuid.NewString()
	send := func(keyID string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqCtx(tid, keyID))
		return rr.Code
	}

	if code := send("key-a"); code != http.StatusOK {
		t.Fatalf("key-a first: got %d, want 200", code)
	}
	if code := send("key-a"); code != http.StatusTooManyRequests {
		t.Fatalf("key-a second: got %d, want 429 (per-key bucket exhausted)", code)
	}
	// A different key under the same tenant is NOT limited by key-a's bucket.
	if code := send("key-b"); code != http.StatusOK {
		t.Fatalf("key-b first: got %d, want 200 (per-key isolation)", code)
	}
}

// The per-tenant ceiling limits the aggregate across keys: even fresh keys are
// refused once the tenant bucket is drained.
func TestMiddlewarePerTenantCeiling(t *testing.T) {
	now := time.Unix(0, 0)
	// Tenant burst=1 so the tenant ceiling bites; per-key burst large so it never does.
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 100, TenantBurst: 1}
	h := mw.Handler(okHandler())

	tid := uuid.NewString()
	send := func(keyID string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqCtx(tid, keyID))
		return rr.Code
	}

	if code := send("key-a"); code != http.StatusOK {
		t.Fatalf("key-a: got %d, want 200", code)
	}
	// Different key, but the tenant ceiling is already drained.
	if code := send("key-b"); code != http.StatusTooManyRequests {
		t.Fatalf("key-b: got %d, want 429 (per-tenant ceiling)", code)
	}
}

// Different tenants do not limit each other.
func TestMiddlewarePerTenantIsolation(t *testing.T) {
	now := time.Unix(0, 0)
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 1}
	h := mw.Handler(okHandler())

	send := func(tid, keyID string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqCtx(tid, keyID))
		return rr.Code
	}

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	if code := send(tenantA, "key-a"); code != http.StatusOK {
		t.Fatalf("tenantA: got %d, want 200", code)
	}
	if code := send(tenantA, "key-a"); code != http.StatusTooManyRequests {
		t.Fatalf("tenantA second: got %d, want 429", code)
	}
	if code := send(tenantB, "key-b"); code != http.StatusOK {
		t.Fatalf("tenantB: got %d, want 200 (independent of tenantA)", code)
	}
}

// A session (no API key) is limited per tenant only, keyed on the tenant.
func TestMiddlewareSessionNoKeyLimitedPerTenant(t *testing.T) {
	now := time.Unix(0, 0)
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 1}
	h := mw.Handler(okHandler())

	tid := uuid.NewString()
	// No key id in context (session-authenticated request).
	send := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/sources", nil)
		ctx := tenant.WithTenantID(req.Context(), tenant.ID(uuid.MustParse(tid)))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req.WithContext(ctx))
		return rr.Code
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("first: got %d, want 200", code)
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("second: got %d, want 429 (per-tenant limit for a keyless request)", code)
	}
}

// No resolved tenant: nothing to key on. Fail closed with 401 rather than
// silently skipping the limiter (never disable limiting off-spec).
func TestMiddlewareNoTenantFailsClosed(t *testing.T) {
	mw := &Middleware{Limiter: New(nil), Limit: staticLimit(1), Burst: 1}
	called := false
	h := mw.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant: got %d, want 401", rr.Code)
	}
	if called {
		t.Fatal("inner handler must not run without a resolved tenant")
	}
}

// A limit-lookup error fails closed with 429: limiting is never silently
// disabled by a backing-store error (spec-mandated fail-closed).
func TestMiddlewareLimitLookupErrorFailsClosed(t *testing.T) {
	mw := &Middleware{
		Limiter: New(nil),
		Limit:   func(context.Context, string) (int, error) { return 0, errors.New("boom") },
		Burst:   1,
	}
	called := false
	h := mw.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqCtx(uuid.NewString(), "key-1"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("lookup error: got %d, want 429 (fail closed)", rr.Code)
	}
	if called {
		t.Fatal("inner handler must not run when the limit cannot be resolved")
	}
}
