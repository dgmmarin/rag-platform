package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// decodeLine parses a single JSON log line into a map for field assertions.
func decodeLine(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, b)
	}
	return m
}

// TestLoggerEmitsJSONWithService proves Logger produces JSON carrying the
// mandatory base field `service` (SPEC-10 §1).
func TestLoggerEmitsJSONWithService(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	log.Info("hello")

	m := decodeLine(t, buf.Bytes())
	if m["service"] != "ragctl" {
		t.Fatalf("service field: want ragctl, got %v", m["service"])
	}
	if m["msg"] != "hello" {
		t.Fatalf("msg field: want hello, got %v", m["msg"])
	}
	if _, ok := m["level"]; !ok {
		t.Fatal("log line missing level field")
	}
}

// TestLoggerLevelFiltersDebug proves the configured level is honoured: a debug
// line is suppressed at info level.
func TestLoggerLevelFiltersDebug(t *testing.T) {
	var buf bytes.Buffer
	log := Logger("ragctl", slog.LevelInfo, &buf)
	log.Debug("noisy")
	if buf.Len() != 0 {
		t.Fatalf("debug line should be filtered at info level, got %q", buf.String())
	}
}

// TestWithContextInjectsTenantAndRequestID is the AC-mandated test: the
// tenant_id (and request_id) placed in the context appear in emitted log lines.
func TestWithContextInjectsTenantAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := Logger("ragctl", slog.LevelInfo, &buf)

	ctx := ContextWithRequestID(context.Background(), "req-123")
	ctx = ContextWithTenantID(ctx, "acme")

	log := With(ctx, base)
	log.Info("served")

	m := decodeLine(t, buf.Bytes())
	if m["tenant_id"] != "acme" {
		t.Fatalf("tenant_id: want acme, got %v (line=%s)", m["tenant_id"], buf.String())
	}
	if m["request_id"] != "req-123" {
		t.Fatalf("request_id: want req-123, got %v (line=%s)", m["request_id"], buf.String())
	}
}

// TestWithContextOmitsAbsentFields proves With does not emit empty tenant_id /
// request_id keys when the context carries no such identity, keeping lines clean
// (the middleware layer sets them when known).
func TestWithContextOmitsAbsentFields(t *testing.T) {
	var buf bytes.Buffer
	base := Logger("ragctl", slog.LevelInfo, &buf)

	log := With(context.Background(), base)
	log.Info("no identity")

	m := decodeLine(t, buf.Bytes())
	if _, ok := m["tenant_id"]; ok {
		t.Fatalf("tenant_id should be absent when unset, line=%s", buf.String())
	}
	if _, ok := m["request_id"]; ok {
		t.Fatalf("request_id should be absent when unset, line=%s", buf.String())
	}
}

// TestRequestIDRoundTrip proves the context accessor returns what was stored.
func TestRequestIDRoundTrip(t *testing.T) {
	ctx := ContextWithRequestID(context.Background(), "abc")
	if got := RequestIDFromContext(ctx); got != "abc" {
		t.Fatalf("RequestIDFromContext: want abc, got %q", got)
	}
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("RequestIDFromContext on empty context: want \"\", got %q", got)
	}
}
