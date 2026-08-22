//go:build e2e

// STORY-01.4 golden path: driving the real ragctl binary, `serve` loads the
// data-encryption key at startup. With no DEK configured it must fail closed
// (exit 1) with a clear error; with a valid local(age) wrapped DEK it must get
// past DEK loading into the serve stub. Asserted against the real binary, no
// mocks (SPEC-09 §2).
package e2e

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"filippo.io/age"
)

// writeLocalDEK generates an age identity, wraps a 32-byte DEK under it, writes
// the wrapped blob next to a returned age secret key. It is the same shape the
// future `ragctl keys` provisioning will emit.
func writeLocalDEK(t *testing.T) (ageKey, blobPath string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	dek := bytes.Repeat([]byte{0x42}, 32)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := w.Write(dek); err != nil {
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

// dekEnv returns a child environment with any ambient DEK settings removed so
// each case controls exactly what the binary sees.
func dekEnv(extra ...string) []string {
	env := os.Environ()
	for _, k := range []string{"KMS_PROVIDER", "AGE_SECRET_KEY", "DEK_WRAPPED_PATH", "DEK_KEY_VERSION", "AWS_KMS_KEY_ID"} {
		env = withoutEnv(env, k)
	}
	return append(env, extra...)
}

func TestServeFailsClosedWithoutDEK(t *testing.T) {
	bin := buildRagctl(t)
	// A valid local KMS key is configured, but no wrapped DEK blob: startup must
	// fail specifically because the DEK is missing (SPEC-09 §2), not for lack of
	// a KMS key.
	ageKey, _ := writeLocalDEK(t)

	cmd := exec.Command(bin, "serve")
	cmd.Env = dekEnv(
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		// DEK_WRAPPED_PATH intentionally omitted.
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("serve started without a DEK; want fail-closed exit 1\n%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("serve: unexpected error type %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("serve without DEK: want exit 1, got %d\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "dek") {
		t.Fatalf("serve error output should mention the missing DEK, got %q", out)
	}
	if strings.Contains(string(out), ageKey) {
		t.Fatalf("serve leaked the age secret key in its error output: %q", out)
	}
}

// TestServeStartsWithValidLocalDEK proves that with a valid local(age) wrapped
// DEK the serve command gets past DEK loading and actually starts serving
// (STORY-01.6 replaced the STORY-01.1 stub with the scaffolding HTTP server).
// It starts the server on an ephemeral port, waits for it to listen, then
// signals a graceful shutdown. It also asserts the DEK/secret is never echoed.
func TestServeStartsWithValidLocalDEK(t *testing.T) {
	bin := buildRagctl(t)
	ageKey, blob := writeLocalDEK(t)

	addr := freeAddr(t)
	cmd := exec.Command(bin, "serve", "--addr", addr)
	cmd.Env = dekEnv(
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		"DEK_WRAPPED_PATH="+blob,
		"DEK_KEY_VERSION=1",
		// serve mounts the SPEC-07 §1 control-plane router (STORY-04.1), which needs a
		// control-plane pool; point it at the running local stack so serve starts. This
		// test still asserts DEK loading + graceful serve + no secret leak.
		"CONTROL_PLANE_URL="+controlPlaneURL(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	waitReachable(t, addr)
	// Complete a request so the server is fully serving (and its signal handler
	// installed) before we ask it to shut down.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v\n%s", err, out.String())
	}
	_ = resp.Body.Close()

	// A valid DEK means the server got past the fail-closed check and is serving.
	// The secret must never appear in the process output.
	if strings.Contains(out.String(), ageKey) {
		t.Fatalf("serve leaked the age secret key in its output: %q", out.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal serve: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		// A clean SIGTERM shutdown exits 0; a non-zero code here is a real failure.
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("serve did not shut down cleanly: exit %d\n%s", exitErr.ExitCode(), out.String())
		}
		t.Fatalf("serve wait: %v\n%s", err, out.String())
	}
}
