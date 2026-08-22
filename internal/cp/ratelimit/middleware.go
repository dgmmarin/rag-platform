package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// LimitFunc resolves a tenant's requests-per-second ceiling (settings.limits.qps,
// SPEC-02 §5 / SPEC-07 §1). It reads only the control-plane settings JSON, never
// tenant data (C-3). An error means the limit could not be resolved and the
// middleware fails closed.
type LimitFunc func(ctx context.Context, tenantID string) (qps int, err error)

// Middleware enforces the SPEC-07 §1 rate limit: a token bucket per API key and
// per tenant, both keyed off the authenticated credential + resolved tenant in
// context (FR-ACC-03), never a client-supplied value. A request must pass BOTH
// buckets; on refusal it returns 429 with Retry-After and RateLimit-* headers
// (NFR-SEC-07). It must run after tenant resolution and API-key authentication.
type Middleware struct {
	// Limiter holds the per-key/per-tenant buckets.
	Limiter *Limiter
	// Limit resolves the tenant's qps ceiling.
	Limit LimitFunc
	// Burst is the per-KEY bucket capacity as a multiple of qps (allowing a short
	// spike above the steady rate). A value <=0 defaults to 1 (burst == qps).
	Burst float64
	// TenantBurst is the per-TENANT bucket capacity as a multiple of qps. The
	// tenant ceiling aggregates across the tenant's keys/sessions, so it is sized
	// looser than a single key (ADR-0026). A value <=0 defaults to 1.
	TenantBurst float64
	// Rejected counts requests refused with 429 (NFR-SEC-07 / SPEC-07 §1
	// "metrics"). Optional: a nil counter is safe (metrics disabled).
	Rejected prometheus.Counter
}

func mult(rate, factor float64) float64 {
	if factor <= 0 {
		factor = 1
	}
	return rate * factor
}

// Handler wraps next with rate limiting.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenant.TenantIDFromCtx(r.Context())
		if !ok {
			// No tenant resolved: nothing to key on. Fail closed rather than
			// letting an unkeyed request bypass the limiter (FR-ACC-03).
			writeError(w, http.StatusUnauthorized, "no tenant resolved")
			return
		}
		tenantID := tid.String()

		qps, err := m.Limit(r.Context(), tenantID)
		if err != nil || qps <= 0 {
			// The ceiling could not be resolved (or is non-positive): fail closed.
			// Limiting is never silently disabled by a backing-store error.
			m.reject(w, time.Second, 0, 0)
			return
		}
		rate := float64(qps)

		// Per-tenant ceiling (aggregate across the tenant's keys/sessions).
		if ok, retry := m.Limiter.Allow("tenant:"+tenantID, rate, mult(rate, m.TenantBurst)); !ok {
			m.reject(w, retry, qps, 0)
			return
		}
		// Per-key bucket, when the request carried an API key (FR-ACC-03: the key
		// id came from the authenticated credential). Session requests have none
		// and are limited by the tenant bucket only.
		if keyID, ok := auth.KeyIDFromCtx(r.Context()); ok && keyID != "" {
			if ok, retry := m.Limiter.Allow("key:"+keyID, rate, mult(rate, m.Burst)); !ok {
				m.reject(w, retry, qps, 0)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// reject writes the 429 response with the standard headers.
func (m *Middleware) reject(w http.ResponseWriter, retry time.Duration, limit, remaining int) {
	if m.Rejected != nil {
		m.Rejected.Inc()
	}
	secs := int(retry / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("RateLimit-Reset", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
}

// writeError writes the SPEC-07 error envelope shape. The rate_limited code
// matches the SPEC-07 §1 error-code list.
func writeError(w http.ResponseWriter, status int, msg string) {
	code := "internal"
	switch status {
	case http.StatusTooManyRequests:
		code = "rate_limited"
	case http.StatusUnauthorized:
		code = "unauthorized"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
