//go:build e2e

// This file holds the STORY-01.2 golden path: with the local stack up
// (`mise run up`, which the e2e task depends on), the app can reach
// Postgres+pgvector, MinIO and the parsing sidecar stub — against the real
// services, no mocks.
package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hostPort returns the effective host port for a service, honouring the same
// ${VAR:-default} override the compose file uses so the test tracks whatever
// ports the developer picked to dodge collisions (e.g. MINIO_PORT).
func hostPort(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// freeAddr asks the OS for an unused loopback TCP port and returns a
// host:port the server can bind. It closes the probe listener before returning,
// so there is a small race window, but it avoids hardcoded ports that collide
// with other local services (the whole reason the compose ports are overridable).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitReachable dials host:port until it accepts a TCP connection or the
// deadline passes. It proves the service is published on the host — i.e. the
// app (running on the host via mprocs) can reach it.
func waitReachable(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("service at %s not reachable: %v", addr, lastErr)
}

// TestPostgresPgvectorReachable proves Postgres is up and the pgvector
// extension the retrieval layer depends on (ADR-0004) is installed in the
// control-plane database. It runs psql inside the container so the assertion
// does not pull a database driver into this infra story.
func TestPostgresPgvectorReachable(t *testing.T) {
	port := hostPort("POSTGRES_PORT", "5432")
	waitReachable(t, net.JoinHostPort("localhost", port))

	user := hostPort("POSTGRES_USER", "rag")
	db := hostPort("POSTGRES_DB", "control_plane")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "exec", "-T", "postgres",
		"psql", "-U", user, "-d", db, "-tAc",
		"select extname from pg_extension where extname = 'vector'")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql pgvector check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "vector") {
		t.Fatalf("pgvector extension not installed in %s; got %q", db, out)
	}
}

// TestMinioReachable proves MinIO is published on the host and healthy via its
// liveness endpoint.
func TestMinioReachable(t *testing.T) {
	port := hostPort("MINIO_PORT", "9000")
	addr := net.JoinHostPort("localhost", port)
	waitReachable(t, addr)

	resp, err := http.Get("http://" + addr + "/minio/health/live")
	if err != nil {
		t.Fatalf("minio health request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("minio health: want 200, got %d", resp.StatusCode)
	}
}

// TestParserSidecarReachable proves the parsing sidecar stub is up and answers
// its health endpoint (SPEC-05 §3 contract; real parser is STORY-05.3).
func TestParserSidecarReachable(t *testing.T) {
	port := hostPort("PARSER_PORT", "8081")
	addr := net.JoinHostPort("localhost", port)
	waitReachable(t, addr)

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("parser health request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("parser health: want 200, got %d", resp.StatusCode)
	}
}
