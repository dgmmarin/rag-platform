package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestMain keeps the cli unit tests hermetic: the "RequiresURL" tests assert a
// command fails closed when no connection URL is set, but kong resolves these
// from the environment (env tags), so a developer's shell exporting them would
// mask the precondition. Strip them once for the whole test binary. Tests that
// need a URL pass it explicitly via --control-plane-url.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("CONTROL_PLANE_URL")
	_ = os.Unsetenv("PROVISION_DB_URL")
	os.Exit(m.Run())
}

// work is no longer a stub: STORY-09.1 wired it to the River worker. Like serve it
// loads the startup DEK and fails closed without one, so it must NOT return
// ErrNotImplemented. Its full consume/drain behaviour is covered by the worker e2e
// (test/e2e/worker_e2e_test.go). Traces: ADR-0005, SPEC-08 §1.
func TestWorkIsImplementedAndNotAStub(t *testing.T) {
	// Force a deterministic fail-closed at the startup DEK load (local KMS, no key)
	// so the worker never actually starts and blocks — regardless of ambient env.
	t.Setenv("KMS_PROVIDER", "local")
	t.Setenv("AGE_SECRET_KEY", "")
	var stdout, stderr bytes.Buffer
	err := Run([]string{"work"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want a startup error when no DEK/control-plane URL is set, got nil")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatalf("work is implemented (STORY-09.1); must not return ErrNotImplemented (stderr=%q)", stderr.String())
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

// The eval commands (STORY-12.1, FR-ADM-04) reach tenant content through the
// resolver, so each needs a control-plane URL to look up the tenant and build the
// resolver. With none set they must fail closed with a clear, actionable error
// mentioning the missing URL — before any DEK load or database dial — and never
// return ErrNotImplemented. (import is excluded here: its --file flag is validated
// by kong at parse time, so it cannot reach the URL check without a real file.)
func TestEvalCommandsRequireURL(t *testing.T) {
	cases := [][]string{
		{"eval", "add", "--slug", "acme", "--question", "why?"},
		{"eval", "list", "--slug", "acme"},
		{"eval", "edit", "--slug", "acme", "--id", "11111111-1111-1111-1111-111111111111"},
		{"eval", "rm", "--slug", "acme", "--id", "11111111-1111-1111-1111-111111111111"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := Run(args, &stdout, &stderr)
			if err == nil {
				t.Fatal("want an error when no control-plane URL is set, got nil")
			}
			if errors.Is(err, ErrNotImplemented) {
				t.Fatal("eval commands are implemented; must not return ErrNotImplemented")
			}
			if !strings.Contains(err.Error(), "control-plane URL") {
				t.Fatalf("error %q should mention the missing control-plane URL", err.Error())
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
	// Fail closed at the DEK load so `work` never starts the worker (see above).
	t.Setenv("KMS_PROVIDER", "local")
	t.Setenv("AGE_SECRET_KEY", "")
	var stdout, stderr bytes.Buffer
	// `work` exercises global-flag acceptance: with the flags parsed it proceeds into
	// Run and fails closed on the missing startup DEK — which proves the grammar
	// accepted the globals (a rejected flag would surface as a usage error before Run
	// is ever reached).
	err := Run([]string{"--log-level", "debug", "--control-plane-url", "postgres://x", "work"}, &stdout, &stderr)
	if err == nil {
		t.Fatalf("want a fail-closed startup error after accepting the globals, got nil (stderr=%q)", stderr.String())
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatalf("unexpected ErrNotImplemented with global flags set (stderr=%q)", stderr.String())
	}
	if strings.Contains(err.Error(), "usage error") {
		t.Fatalf("global flags were rejected as a usage error: %v", err)
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
