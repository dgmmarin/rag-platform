//go:build e2e

// Package e2e holds end-to-end tests that drive the real ragctl binary. For
// STORY-01.1 the golden path is: build ragctl, invoke each subcommand, and
// assert it runs (the stubs exit 2 with a recognisable line). Later stories add
// e2e tests that require the local stack (Postgres+pgvector, MinIO).
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
		// serve now loads the DEK at startup (STORY-01.4). Its golden paths live
		// in secrets_e2e_test.go; with no DEK configured it fails closed (exit 1).
		{name: "work", args: []string{"work"}, wantCode: 2, wantOut: "work"},
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
