package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Each subcommand is a stub for STORY-01.1: the CLI wiring must be real, but the
// command bodies return ErrNotImplemented and print a recognisable line. Traces:
// ADR-0002, ADR-0009, SPEC-02 §7.
func TestSubcommandsAreWiredAndReturnNotImplemented(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring expected on stdout
	}{
		// serve is no longer a pure stub: it loads the DEK at startup and fails
		// closed without one (STORY-01.4); see secrets_test.go for its contract.
		{"work", []string{"work"}, "work"},
		// migrate control (STORY-01.5), migrate tenants (STORY-02.2) and enroll
		// (STORY-02.3) are implemented; see their *RequiresURL tests.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := Run(tc.args, &stdout, &stderr)
			if !errors.Is(err, ErrNotImplemented) {
				t.Fatalf("args %v: want ErrNotImplemented, got %v (stderr=%q)", tc.args, err, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("args %v: stdout %q does not contain %q", tc.args, got, tc.want)
			}
		})
	}
}

// migrate control is wired to goose (STORY-01.5). Without a resolved
// control-plane URL it must fail with a clear, actionable error rather than
// ErrNotImplemented or a nil-URL panic. It must never be ErrNotImplemented.
func TestMigrateControlRequiresURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"migrate", "control"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want an error when control-plane URL is unset, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("migrate control is implemented; must not return ErrNotImplemented")
	}
	if !strings.Contains(err.Error(), "control-plane URL") {
		t.Fatalf("error %q should mention the missing control-plane URL", err.Error())
	}
}

// migrate tenants is wired to the per-tenant goose runner (STORY-02.2,
// SPEC-01 §7). Without a resolved control-plane URL it must fail with a clear,
// actionable error rather than ErrNotImplemented — the fail-closed check runs
// before any DEK load or database dial.
func TestMigrateTenantsRequiresURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"migrate", "tenants"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want an error when control-plane URL is unset, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("migrate tenants is implemented; must not return ErrNotImplemented")
	}
	if !strings.Contains(err.Error(), "control-plane URL") {
		t.Fatalf("error %q should mention the missing control-plane URL", err.Error())
	}
}

// enroll is wired to the provisioner (STORY-02.3, SPEC-01 §6). With no resolved
// control-plane / provisioning URL it must fail with a clear, actionable error
// rather than ErrNotImplemented — the fail-closed check runs before any DEK load
// or database dial.
func TestEnrollRequiresURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"enroll", "--slug", "acme", "--name", "Acme Inc"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want an error when no provisioning URL is set, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("enroll is implemented; must not return ErrNotImplemented")
	}
	if !strings.Contains(err.Error(), "URL") {
		t.Fatalf("error %q should mention the missing connection URL", err.Error())
	}
}

// The tenant lifecycle commands (STORY-02.4, SPEC-01 §8) are wired to the
// Lifecycle service. With no resolved provisioning/control-plane URL each must
// fail with a clear, actionable error rather than ErrNotImplemented — the
// fail-closed check runs before any database dial.
func TestTenantLifecycleCommandsRequireURL(t *testing.T) {
	cases := [][]string{
		{"tenant", "suspend", "--slug", "acme"},
		{"tenant", "resume", "--slug", "acme"},
		{"tenant", "delete", "--slug", "acme"},
		{"tenant", "delete", "--slug", "acme", "--cancel"},
		{"tenant", "delete", "--slug", "acme", "--run"},
		{"tenant", "move", "--slug", "acme", "--db-host", "pg-2"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := Run(args, &stdout, &stderr)
			if err == nil {
				t.Fatal("want an error when no provisioning URL is set, got nil")
			}
			if errors.Is(err, ErrNotImplemented) {
				t.Fatal("tenant lifecycle commands are implemented; must not return ErrNotImplemented")
			}
			if !strings.Contains(err.Error(), "URL") {
				t.Fatalf("error %q should mention the missing connection URL", err.Error())
			}
		})
	}
}

// A scheduled delete and its cancellation are mutually exclusive; the grammar
// must reject asking for both at once rather than silently picking one.
func TestTenantDeleteRejectsCancelAndRunTogether(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"--control-plane-url", "postgres://x", "tenant", "delete",
		"--slug", "acme", "--cancel", "--run"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want an error when --cancel and --run are both set, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("must not resolve to ErrNotImplemented")
	}
}

// Global flags resolve flag -> env -> config file (ADR-0009). At minimum the
// grammar must accept the documented global flags without error.
func TestGlobalFlagsAreAccepted(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// Use `work` (a pure stub) so the assertion is about global-flag acceptance,
	// not serve's DEK requirement (STORY-01.4).
	err := Run([]string{"--log-level", "debug", "--control-plane-url", "postgres://x", "work"}, &stdout, &stderr)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented with global flags set, got %v (stderr=%q)", err, stderr.String())
	}
}

// --help is handled by Kong: it prints usage and Run reports ErrHelpRequested,
// which the entrypoint treats as a clean exit.
func TestHelpIsReportedAsHelpRequested(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"--help"}, &stdout, &stderr)
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("want ErrHelpRequested, got %v", err)
	}
	if !strings.Contains(stdout.String(), "ragctl") {
		t.Fatalf("help output %q does not mention ragctl", stdout.String())
	}
}

// An unknown subcommand must be a parse error, never ErrNotImplemented.
func TestUnknownSubcommandErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run([]string{"bogus"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want a parse error for unknown subcommand, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("unknown subcommand must not resolve to ErrNotImplemented")
	}
}
