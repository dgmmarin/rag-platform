package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads a prometheus.Counter's current value without pulling in the
// testutil helper (which adds a new indirect dependency).
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// A rejected request increments the rate_limited counter; an allowed one does not.
func TestMiddlewareIncrementsRejectedMetric(t *testing.T) {
	now := time.Unix(0, 0)
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "api_rate_limited_total"})
	reg.MustRegister(c)

	mw := &Middleware{
		Limiter:  New(func() time.Time { return now }),
		Limit:    staticLimit(1),
		Burst:    1,
		Rejected: c,
	}
	h := mw.Handler(okHandler())

	tid := uuid.NewString()
	do := func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqCtx(tid, "key-1"))
	}
	do() // allowed
	if got := counterValue(t, c); got != 0 {
		t.Fatalf("counter after allowed request = %v, want 0", got)
	}
	do() // rejected
	if got := counterValue(t, c); got != 1 {
		t.Fatalf("counter after rejected request = %v, want 1", got)
	}
}

// A nil metric is safe (no panic) — metrics are optional. The middleware still
// admits then refuses correctly with no counter wired.
func TestMiddlewareNilMetricSafe(t *testing.T) {
	now := time.Unix(0, 0)
	mw := &Middleware{Limiter: New(func() time.Time { return now }), Limit: staticLimit(1), Burst: 1}
	h := mw.Handler(okHandler())
	tid := uuid.NewString()

	first := httptest.NewRecorder()
	h.ServeHTTP(first, reqCtx(tid, "key-1"))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, reqCtx(tid, "key-1"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429 (nil metric must not affect limiting)", second.Code)
	}
}
