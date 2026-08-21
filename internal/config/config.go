// Package config loads platform configuration from the environment with an
// optional config-file overlay (STORY-01.4, SPEC-09 §2). It follows the CLI's
// documented flag -> env -> config-file precedence (ADR-0009): the config file
// supplies values, the environment overrides the file, and an explicit CLI flag
// (resolved by Kong upstream) overrides both.
//
// Only the settings this story needs live here — the KMS/DEK wiring for secret
// loading. Unrelated configuration is added by the stories that need it.
//
// Secret material (age keys, wrapped DEK) is referenced by path/handle here and
// never printed; nothing in this package logs its values.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the resolved platform configuration for secret loading.
type Config struct {
	// KMSProvider selects how the DEK is wrapped: "local" (age) or "aws".
	KMSProvider string
	// AgeSecretKey is the age X25519 secret key wrapping the DEK for the local
	// provider (AGE-SECRET-KEY-1...). Held outside the database (C-4).
	AgeSecretKey string
	// AWSKMSKeyID is the AWS KMS key id/ARN wrapping the DEK for the aws provider.
	AWSKMSKeyID string
	// DEKWrappedPath is the path to the wrapped-DEK blob on disk.
	DEKWrappedPath string
	// DEKKeyVersion is the active DEK generation; new ciphertext is sealed under
	// it, and rotation increments it (SPEC-09 §2).
	DEKKeyVersion uint16

	// LogLevel is the textual slog level (debug, info, warn, error). Empty means
	// info (STORY-01.6, SPEC-10 §1).
	LogLevel string
	// OTLPEndpoint is the OpenTelemetry OTLP/gRPC collector endpoint (host:port).
	// Empty disables tracing so local dev needs nothing (SPEC-10 §3).
	OTLPEndpoint string
	// OTLPInsecure sends traces over plaintext gRPC (typical for a local
	// collector).
	OTLPInsecure bool
	// TraceSamplerRatio is the parent-based ratio sampler fraction (0..1) applied
	// when an endpoint is configured; <=0 falls back to always-sample.
	TraceSamplerRatio float64
}

// Load reads configuration, overlaying the optional config file (if filePath is
// non-empty) with the process environment. A file value is applied only when
// the same variable is absent from the environment, implementing env-over-file
// precedence.
func Load(filePath string) (Config, error) {
	get := envLookup
	if filePath != "" {
		fileVals, err := readEnvFile(filePath)
		if err != nil {
			return Config{}, err
		}
		get = func(key string) (string, bool) {
			if v, ok := envLookup(key); ok {
				return v, true
			}
			v, ok := fileVals[key]
			return v, ok
		}
	}

	cfg := Config{
		KMSProvider:    firstNonEmpty(mustGet(get, "KMS_PROVIDER"), "local"),
		AgeSecretKey:   mustGet(get, "AGE_SECRET_KEY"),
		AWSKMSKeyID:    mustGet(get, "AWS_KMS_KEY_ID"),
		DEKWrappedPath: mustGet(get, "DEK_WRAPPED_PATH"),
		DEKKeyVersion:  1,
	}

	if raw := mustGet(get, "DEK_KEY_VERSION"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid DEK_KEY_VERSION %q: %w", raw, err)
		}
		cfg.DEKKeyVersion = uint16(v)
	}

	// Observability (STORY-01.6, SPEC-10). The OTLP endpoint follows the standard
	// OTEL_EXPORTER_OTLP_ENDPOINT variable; empty leaves tracing disabled.
	cfg.LogLevel = firstNonEmpty(mustGet(get, "LOG_LEVEL"), "info")
	cfg.OTLPEndpoint = mustGet(get, "OTEL_EXPORTER_OTLP_ENDPOINT")
	cfg.OTLPInsecure = mustGet(get, "OTEL_EXPORTER_OTLP_INSECURE") == "true"
	if raw := mustGet(get, "OTEL_TRACES_SAMPLER_RATIO"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid OTEL_TRACES_SAMPLER_RATIO %q: %w", raw, err)
		}
		cfg.TraceSamplerRatio = v
	}

	return cfg, nil
}

// envLookup returns an environment value, treating an empty value as unset so an
// exported-but-empty variable falls through to the file overlay or default.
func envLookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// mustGet returns the value for key or "" when unset.
func mustGet(get func(string) (string, bool), key string) string {
	v, _ := get(key)
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readEnvFile parses a minimal KEY=VALUE file (one per line, # comments, blank
// lines ignored). It is deliberately small: the config file is an operator
// convenience, not a general templating language.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	vals := make(map[string]string)
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, val, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("config: %s:%d: expected KEY=VALUE, got %q", path, line, text)
		}
		vals[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return vals, nil
}
