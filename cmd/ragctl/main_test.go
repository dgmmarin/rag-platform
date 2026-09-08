package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/cli"
)

// The entrypoint maps Run's error sentinels to the ADR-0010 exit-code contract:
// 0 (clean/help), 2 (wired-but-unimplemented stub), 1 (any real error). `work`
// was the last STORY-01.1 stub and is now implemented (STORY-09.1), so no live
// command returns ErrNotImplemented — codeFor is tested directly so the exit-2
// mapping stays guarded (ADR-0010: CI gates on exit codes, not text). Traces:
// ADR-0010, ADR-0009.
func TestCodeForMapsErrorsToExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"help printed", cli.ErrHelpRequested, 0},
		{"unimplemented stub", cli.ErrNotImplemented, 2},
		{"wrapped stub sentinel", fmt.Errorf("work: %w", cli.ErrNotImplemented), 2},
		{"real error", errors.New("boom"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeFor(tc.err); got != tc.want {
				t.Fatalf("codeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
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
