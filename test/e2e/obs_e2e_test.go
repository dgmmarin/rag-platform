//go:build e2e

// STORY-01.6 golden path: driving the real ragctl binary, `serve` starts the
// scaffolding HTTP server (obs middleware + /healthz, /readyz, /metrics) once
// the DEK loads. This proves the observability wiring end to end against the real
// binary: liveness/readiness respond 200, /metrics exposes the request histogram
// in Prometheus text, and a served request produces a slog JSON line carrying a
// request_id (SPEC-10 §1/§2/§4). No mocks, no local infra needed for this path.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"

	"filippo.io/age"
	"os"
	"os/exec"
	"path/filepath"
)

// wrapLocalDEK writes a wrapped DEK next to a returned age secret key (same shape
// as writeLocalDEK in secrets_e2e_test.go, duplicated here to keep the files
// independent).
func wrapLocalDEK(t *testing.T) (ageKey, blobPath string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := w.Write(bytes.Repeat([]byte{0x11}, 32)); err != nil {
		t.Fatalf("write dek: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	blobPath = filepath.Join(t.TempDir(), "dek.age")
	if err := os.WriteFile(blobPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return id.String(), blobPath
}

// captureWriter is a concurrency-safe buffer so the test can read the server's
// stderr log stream while the process runs.
type captureWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureWriter) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func TestServeObservabilityGoldenPath(t *testing.T) {
	bin := buildRagctl(t)
	ageKey, blob := wrapLocalDEK(t)

	addr := freeAddr(t)
	cmd := exec.Command(bin, "serve", "--addr", addr)
	cmd.Env = dekEnv(
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		"DEK_WRAPPED_PATH="+blob,
		"DEK_KEY_VERSION=1",
		"LOG_LEVEL=info",
		// No OTLP endpoint: tracing is disabled, nothing external required.
	)
	logs := &captureWriter{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	waitReachable(t, addr)
	base := "http://" + addr

	// /healthz -> 200
	if code := getStatus(t, base+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz: want 200, got %d\n%s", code, logs.String())
	}
	// /readyz -> 200 with the skeleton JSON shape.
	readyBody := getBody(t, base+"/readyz")
	var ready struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(readyBody, &ready); err != nil {
		t.Fatalf("/readyz body not JSON: %v\n%s", err, readyBody)
	}
	if !ready.Ready || ready.Checks == nil {
		t.Fatalf("/readyz skeleton unexpected: %s", readyBody)
	}
	// /metrics -> 200 exposing the request histogram in Prometheus text.
	metricsBody := getBody(t, base+"/metrics")
	if !strings.Contains(string(metricsBody), "api_request_duration_seconds") {
		t.Fatalf("/metrics missing api_request_duration_seconds:\n%s", metricsBody)
	}

	// A served request must have produced at least one slog JSON line carrying a
	// non-empty request_id (SPEC-10 §1). Scan the captured stderr for it.
	if !hasRequestIDLogLine(logs.String()) {
		t.Fatalf("no slog line with a request_id found in server logs:\n%s", logs.String())
	}

	// The secret must never be echoed.
	if strings.Contains(logs.String(), ageKey) {
		t.Fatalf("serve leaked the age secret key in its logs: %q", logs.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal serve: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("serve did not shut down cleanly: exit %d\n%s", exitErr.ExitCode(), logs.String())
		}
		t.Fatalf("serve wait: %v", err)
	}
}

// hasRequestIDLogLine reports whether any JSON log line carries a non-empty
// request_id field.
func hasRequestIDLogLine(logs string) bool {
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if id, ok := m["request_id"].(string); ok && id != "" {
			return true
		}
	}
	return false
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func getBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return b
}
