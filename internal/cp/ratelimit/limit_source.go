package ratelimit

import (
	"context"
	"time"
)

// SettingsSource reads a tenant's settings document (the SPEC-02 §5 view, which
// carries limits.qps). *tenants.SettingsService satisfies this via its Get
// method. It reads only control-plane settings JSON, never tenant data (C-3).
type SettingsSource interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// LimitFromSettings builds a LimitFunc that reads settings.limits.qps for a
// tenant. A store error propagates so the middleware fails closed; a missing or
// malformed qps falls back to defaultQPS (a floor, never "unlimited").
func LimitFromSettings(src SettingsSource, defaultQPS int) LimitFunc {
	return func(ctx context.Context, tenantID string) (int, error) {
		doc, err := src.Get(ctx, tenantID)
		if err != nil {
			return 0, err
		}
		if qps, ok := qpsFromDoc(doc); ok {
			return qps, nil
		}
		return defaultQPS, nil
	}
}

// qpsFromDoc extracts limits.qps, accepting the int or the float64 a JSON number
// decodes to. Returns false when absent or non-positive so the caller applies
// its default.
func qpsFromDoc(doc map[string]any) (int, bool) {
	limits, ok := doc["limits"].(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := limits["qps"].(type) {
	case int:
		if v > 0 {
			return v, true
		}
	case int64:
		if v > 0 {
			return int(v), true
		}
	case float64:
		if v > 0 {
			return int(v), true
		}
	}
	return 0, false
}

// Run sweeps idle buckets on the given interval until ctx is cancelled, bounding
// the limiter's memory (idle buckets hold no state that matters — a re-created
// bucket starts full). A non-positive interval defaults to one minute.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.evictIdle()
		}
	}
}
