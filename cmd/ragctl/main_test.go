package main

import (
	"bytes"
	"strings"
	"testing"
)

// The entrypoint maps a stub subcommand (ErrNotImplemented) to exit code 2 so
// callers and the e2e test can distinguish "wired but unimplemented" from a
// clean run (0) and a parse/usage error. Traces: STORY-01.1, ADR-0009.
func TestRunStubReturnsExitCode2(t *testing.T) {
	// `work` remains a pure stub; `serve` now loads the DEK at startup
	// (STORY-01.4) and so is no longer a not-implemented-only path.
	var stdout, stderr bytes.Buffer
	code := run([]string{"work"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("stub subcommand: want exit code 2, got %d (stderr=%q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "work") {
		t.Fatalf("expected work stub output, got %q", stdout.String())
	}
}

func TestRunHelpReturnsExitCode0(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--help: want exit code 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "ragctl") {
		t.Fatalf("--help: expected usage on stdout, got %q", stdout.String())
	}
}

func TestRunUnknownSubcommandReturnsExitCode1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("unknown subcommand: want exit code 1, got %d", code)
	}
}

// A command that returns a real error (exit 1) must surface the message on
// stderr; Kong prints its own parse errors, but a command Run error would
// otherwise vanish. migrate control with no control-plane URL is the concrete
// case (STORY-01.5).
func TestRunCommandErrorIsReportedOnStderr(t *testing.T) {
	t.Setenv("CONTROL_PLANE_URL", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"migrate", "control"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("migrate control without URL: want exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "control-plane URL") {
		t.Fatalf("expected control-plane URL error on stderr, got %q", stderr.String())
	}
}
