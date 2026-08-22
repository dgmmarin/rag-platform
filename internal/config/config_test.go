package config

import (
	"os"
	"path/filepath"
	"testing"
)

// setEnv sets env vars for the duration of a test and clears them afterwards.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// TestLoadFromEnv proves Config values are read from the environment.
func TestLoadFromEnv(t *testing.T) {
	setEnv(t, map[string]string{
		"KMS_PROVIDER":     "local",
		"AGE_SECRET_KEY":   "AGE-SECRET-KEY-EXAMPLE",
		"DEK_WRAPPED_PATH": "/etc/rag/dek.age",
		"DEK_KEY_VERSION":  "5",
	})
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KMSProvider != "local" {
		t.Fatalf("KMSProvider = %q, want local", cfg.KMSProvider)
	}
	if cfg.AgeSecretKey != "AGE-SECRET-KEY-EXAMPLE" {
		t.Fatalf("AgeSecretKey = %q", cfg.AgeSecretKey)
	}
	if cfg.DEKWrappedPath != "/etc/rag/dek.age" {
		t.Fatalf("DEKWrappedPath = %q", cfg.DEKWrappedPath)
	}
	if cfg.DEKKeyVersion != 5 {
		t.Fatalf("DEKKeyVersion = %d, want 5", cfg.DEKKeyVersion)
	}
}

// TestDefaults proves sensible defaults when env is unset: local KMS, version 1.
func TestDefaults(t *testing.T) {
	// Ensure a clean environment for the keys under test. An empty value is
	// treated as unset by envLookup, and t.Setenv restores the prior value after
	// the test.
	for _, k := range []string{"KMS_PROVIDER", "DEK_KEY_VERSION", "AGE_SECRET_KEY", "DEK_WRAPPED_PATH"} {
		t.Setenv(k, "")
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KMSProvider != "local" {
		t.Fatalf("default KMSProvider = %q, want local", cfg.KMSProvider)
	}
	if cfg.DEKKeyVersion != 1 {
		t.Fatalf("default DEKKeyVersion = %d, want 1", cfg.DEKKeyVersion)
	}
}

// TestLoadObservabilitySettings proves the observability config (STORY-01.6,
// SPEC-10) is read from the environment, and that tracing defaults to disabled
// (no endpoint) with an info log level.
func TestLoadObservabilitySettings(t *testing.T) {
	setEnv(t, map[string]string{
		"LOG_LEVEL":                   "debug",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4317",
		"OTEL_EXPORTER_OTLP_INSECURE": "true",
		"OTEL_TRACES_SAMPLER_RATIO":   "0.25",
	})
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.OTLPEndpoint != "collector:4317" {
		t.Fatalf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}
	if !cfg.OTLPInsecure {
		t.Fatal("OTLPInsecure should be true")
	}
	if cfg.TraceSamplerRatio != 0.25 {
		t.Fatalf("TraceSamplerRatio = %v, want 0.25", cfg.TraceSamplerRatio)
	}
}

func TestObservabilityDefaultsDisabled(t *testing.T) {
	for _, k := range []string{"LOG_LEVEL", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_INSECURE", "OTEL_TRACES_SAMPLER_RATIO"} {
		t.Setenv(k, "")
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OTLPEndpoint != "" {
		t.Fatalf("OTLPEndpoint should default empty (tracing disabled), got %q", cfg.OTLPEndpoint)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("LogLevel default = %q, want info", cfg.LogLevel)
	}
}

// TestFileOverlayThenEnvWins proves the documented flag->env->file precedence:
// a value present in the environment overrides the same key in the config file,
// while a key only in the file is still applied.
func TestFileOverlayThenEnvWins(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.env")
	if err := os.WriteFile(file, []byte(
		"KMS_PROVIDER=aws\nAWS_KMS_KEY_ID=arn:from-file\nDEK_KEY_VERSION=9\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// KMS_PROVIDER is set in the env and must win over the file's value.
	t.Setenv("KMS_PROVIDER", "local")
	// AWS_KMS_KEY_ID and DEK_KEY_VERSION are only in the file; clear any ambient
	// value (empty is treated as unset) so the file overlay is what's exercised.
	t.Setenv("AWS_KMS_KEY_ID", "")
	t.Setenv("DEK_KEY_VERSION", "")

	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KMSProvider != "local" {
		t.Fatalf("env did not win: KMSProvider = %q, want local", cfg.KMSProvider)
	}
	if cfg.AWSKMSKeyID != "arn:from-file" {
		t.Fatalf("file-only key not applied: AWSKMSKeyID = %q", cfg.AWSKMSKeyID)
	}
	if cfg.DEKKeyVersion != 9 {
		t.Fatalf("file-only DEKKeyVersion = %d, want 9", cfg.DEKKeyVersion)
	}
}

// TestMissingFileIsError proves a config path that does not exist is a clear
// error, not silently ignored.
func TestMissingFileIsError(t *testing.T) {
	if _, err := Load("/no/such/config.env"); err == nil {
		t.Fatal("Load accepted a nonexistent config file path")
	}
}

// TestBadKeyVersionIsError proves a non-numeric DEK_KEY_VERSION fails loudly.
func TestBadKeyVersionIsError(t *testing.T) {
	t.Setenv("DEK_KEY_VERSION", "not-a-number")
	if _, err := Load(""); err == nil {
		t.Fatal("Load accepted a non-numeric DEK_KEY_VERSION")
	}
}

// TestLoadProvisioningConfig proves the privileged provisioning connection and
// the tenant-facing DB host/port are read from the environment (STORY-02.3,
// SPEC-01 §6). The privileged URL is a superuser connection used only for
// CREATE ROLE/DATABASE/EXTENSION; it is never logged.
func TestLoadProvisioningConfig(t *testing.T) {
	setEnv(t, map[string]string{
		"PROVISION_DB_URL": "postgres://super:pw@dbhost:5432/postgres?sslmode=disable",
		"TENANT_DB_HOST":   "pg-1.internal",
		"TENANT_DB_PORT":   "6432",
	})
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProvisionURL != "postgres://super:pw@dbhost:5432/postgres?sslmode=disable" {
		t.Fatalf("ProvisionURL = %q", cfg.ProvisionURL)
	}
	if cfg.TenantDBHost != "pg-1.internal" {
		t.Fatalf("TenantDBHost = %q", cfg.TenantDBHost)
	}
	if cfg.TenantDBPort != 6432 {
		t.Fatalf("TenantDBPort = %d, want 6432", cfg.TenantDBPort)
	}
}

// TestBadTenantDBPortIsError proves a non-numeric TENANT_DB_PORT fails loudly.
func TestBadTenantDBPortIsError(t *testing.T) {
	t.Setenv("TENANT_DB_PORT", "not-a-port")
	if _, err := Load(""); err == nil {
		t.Fatal("Load accepted a non-numeric TENANT_DB_PORT")
	}
}
