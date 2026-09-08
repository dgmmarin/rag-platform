//go:build e2e

// Package e2e holds end-to-end tests that drive the real ragctl binary. The
// golden path builds ragctl and invokes each subcommand, asserting the stable
// exit-code contract (ADR-0010): implemented commands fail closed with exit 1 and
// a recognisable message when their required inputs are missing; only a
// wired-but-unimplemented stub exits 2 (none remain). Later stories add e2e tests
// that require the local stack (Postgres+pgvector, MinIO).
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withoutEnv returns env with any entries for key (KEY=...) removed.
func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	prefix := key + "="
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// buildRagctl compiles the binary from source into the test temp dir and
// returns its path, exercising the same `go build` the mise build task runs.
func buildRagctl(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	bin := filepath.Join(t.TempDir(), "ragctl")

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/ragctl")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ragctl: %v\n%s", err, out)
	}
	return bin
}

func TestRagctlGoldenPath(t *testing.T) {
	bin := buildRagctl(t)

	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		// clearControlURL removes CONTROL_PLANE_URL from the child env so the
		// case is deterministic regardless of the developer/CI environment.
		clearControlURL bool
	}{
		// serve and work now load the DEK at startup and then run long-lived
		// (STORY-01.4, STORY-09.1), so neither is a smoke-testable stub any more.
		// serve's golden paths live in secrets_e2e_test.go; work's fail-closed path
		// is TestWorkFailsClosedWithoutDEK below and its real golden path (consuming
		// jobs off Postgres) is worker_e2e_test.go.
		// migrate control is implemented (STORY-01.5): with no CONTROL_PLANE_URL
		// it fails (exit 1) with the missing-URL error, not the exit-2 stub. Its
		// real golden path against Postgres lives in TestMigrateControlAppliesSchema.
		{name: "migrate control", args: []string{"migrate", "control"}, wantCode: 1, wantOut: "control-plane URL", clearControlURL: true},
		// migrate tenants is implemented (STORY-02.2): with no CONTROL_PLANE_URL it
		// fails closed (exit 1) with the missing-URL error, not the exit-2 stub. Its
		// real golden path against Postgres lives in TestMigrateTenantsGoldenPath.
		{name: "migrate tenants", args: []string{"migrate", "tenants"}, wantCode: 1, wantOut: "control-plane URL", clearControlURL: true},
		// enroll is implemented (STORY-02.3): with no CONTROL_PLANE_URL / PROVISION_DB_URL
		// it fails closed (exit 1) with the missing-URL error, not the exit-2 stub. Its
		// real golden path against Postgres lives in TestEnrollProvisionsTenantGoldenPath.
		{name: "enroll", args: []string{"enroll", "--slug", "acme", "--name", "Acme Inc"}, wantCode: 1, wantOut: "provisioning URL", clearControlURL: true},
		{name: "help", args: []string{"--help"}, wantCode: 0, wantOut: "ragctl"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bin, tc.args...)
			if tc.clearControlURL {
				cmd.Env = withoutEnv(withoutEnv(os.Environ(), "CONTROL_PLANE_URL"), "PROVISION_DB_URL")
			}
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("running %v: %v\n%s", tc.args, err, out)
				}
				code = exitErr.ExitCode()
			}
			if code != tc.wantCode {
				t.Fatalf("%v: want exit %d, got %d\n%s", tc.args, tc.wantCode, code, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Fatalf("%v: output %q does not contain %q", tc.args, out, tc.wantOut)
			}
		})
	}
}

// TestWorkFailsClosedWithoutDEK proves the worker binary honours the same
// fail-closed startup as serve (SPEC-09 §2, STORY-09.1): with a valid local KMS
// key but no wrapped DEK blob, `work` must exit 1 before it ever opens the
// control-plane pool or starts consuming jobs, with an error naming the missing
// DEK and never echoing the age secret. dekEnv/writeLocalDEK are shared with the
// serve secrets e2e; no local stack is needed since startup fails first.
func TestWorkFailsClosedWithoutDEK(t *testing.T) {
	bin := buildRagctl(t)
	ageKey, _ := writeLocalDEK(t)

	cmd := exec.Command(bin, "work")
	cmd.Env = dekEnv(
		"KMS_PROVIDER=local",
		"AGE_SECRET_KEY="+ageKey,
		// DEK_WRAPPED_PATH intentionally omitted: the DEK is what's missing.
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("work started without a DEK; want fail-closed exit 1\n%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("work: unexpected error type %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("work without DEK: want exit 1, got %d\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "dek") {
		t.Fatalf("work error output should mention the missing DEK, got %q", out)
	}
	if strings.Contains(string(out), ageKey) {
		t.Fatalf("work leaked the age secret key in its error output: %q", out)
	}
}
