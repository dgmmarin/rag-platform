// Command ragctl is the single binary for the multi-tenant RAG platform. Per
// ADR-0009 and SPEC-02 §7 it both operates the platform (enroll, migrate) and
// starts it (serve, work). STORY-01.1 wires the subcommand skeleton.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rag-platform/ragctl/internal/cli"
)

// run parses args and returns a process exit code. It is separated from main so
// it can be exercised in tests without touching os.Exit.
//
// Exit codes:
//
//	0 — command completed successfully
//	1 — parse/usage error
//	2 — command is wired but not yet implemented (STORY-01.1 stubs)
func run(args []string, stdout, stderr io.Writer) int {
	err := cli.Run(args, stdout, stderr)
	code := codeFor(err)
	// A real command/usage failure (exit 1). Kong prints its own parse errors, but
	// an error surfaced from a command Run (e.g. a missing control-plane URL) would
	// otherwise be lost, so report it on stderr.
	if code == 1 {
		_, _ = fmt.Fprintln(stderr, "ragctl:", err)
	}
	return code
}

// codeFor maps a cli.Run error to the ADR-0010 exit-code contract: 0 for a clean
// run or printed help, 2 for a wired-but-unimplemented stub (ErrNotImplemented),
// 1 for any other (real) error. It stays a pure function so the contract — the
// exit-2 mapping included, now that every command is implemented and no live
// command returns ErrNotImplemented — remains unit-testable without a stub.
func codeFor(err error) int {
	switch {
	case err == nil, errors.Is(err, cli.ErrHelpRequested):
		return 0
	case errors.Is(err, cli.ErrNotImplemented):
		return 2
	default:
		return 1
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
