package documents

import (
	"context"
	"testing"
)

type fakeSettingsReader struct {
	doc map[string]any
	err error
}

func (f fakeSettingsReader) Get(_ context.Context, _ string) (map[string]any, error) {
	return f.doc, f.err
}

func TestSettingsUploadLimitsReadsMaxUploadMB(t *testing.T) {
	l := SettingsUploadLimits{Settings: fakeSettingsReader{doc: map[string]any{
		"limits": map[string]any{"max_upload_mb": float64(10)},
	}}}
	got, err := l.MaxUploadBytes(context.Background(), tid)
	if err != nil {
		t.Fatalf("MaxUploadBytes: %v", err)
	}
	if got != 10<<20 {
		t.Fatalf("MaxUploadBytes = %d, want %d", got, 10<<20)
	}
}

func TestSettingsUploadLimitsMissingLimitFallsThrough(t *testing.T) {
	// No limits.max_upload_mb present: return 0 so the service falls back to the
	// configured global ceiling (fail-safe, never unbounded).
	l := SettingsUploadLimits{Settings: fakeSettingsReader{doc: map[string]any{}}}
	got, err := l.MaxUploadBytes(context.Background(), tid)
	if err != nil {
		t.Fatalf("MaxUploadBytes: %v", err)
	}
	if got != 0 {
		t.Fatalf("MaxUploadBytes (absent) = %d, want 0", got)
	}
}
