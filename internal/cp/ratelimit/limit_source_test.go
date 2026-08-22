package ratelimit

import (
	"context"
	"errors"
	"testing"
)

type fakeSettings struct {
	doc map[string]any
	err error
}

func (f fakeSettings) Get(context.Context, string) (map[string]any, error) {
	return f.doc, f.err
}

// LimitFromSettings extracts settings.limits.qps from the settings document.
func TestLimitFromSettingsReadsQPS(t *testing.T) {
	src := fakeSettings{doc: map[string]any{
		"limits": map[string]any{"qps": 42},
	}}
	lf := LimitFromSettings(src, 10)
	qps, err := lf(context.Background(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qps != 42 {
		t.Fatalf("qps = %d, want 42", qps)
	}
}

// A settings-store error propagates so the middleware fails closed.
func TestLimitFromSettingsPropagatesError(t *testing.T) {
	lf := LimitFromSettings(fakeSettings{err: errors.New("boom")}, 10)
	if _, err := lf(context.Background(), "t1"); err == nil {
		t.Fatal("expected the settings error to propagate")
	}
}

// A missing/malformed qps falls back to the configured default rather than
// disabling limiting (fail closed to a floor).
func TestLimitFromSettingsFallsBackToDefault(t *testing.T) {
	lf := LimitFromSettings(fakeSettings{doc: map[string]any{}}, 7)
	qps, err := lf(context.Background(), "t1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if qps != 7 {
		t.Fatalf("qps = %d, want the default 7", qps)
	}
}

// float64-encoded qps (as JSON numbers decode) is read correctly.
func TestLimitFromSettingsReadsFloatQPS(t *testing.T) {
	lf := LimitFromSettings(fakeSettings{doc: map[string]any{
		"limits": map[string]any{"qps": float64(25)},
	}}, 10)
	qps, _ := lf(context.Background(), "t1")
	if qps != 25 {
		t.Fatalf("qps = %d, want 25", qps)
	}
}
