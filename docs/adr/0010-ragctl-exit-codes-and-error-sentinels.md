# ADR-0010: ragctl exit-code contract and error sentinels

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** ADR-0002, ADR-0009, SPEC-02 §7, NFR-MNT-03

## Context
STORY-01.1 delivers the `ragctl` subcommand skeleton: the Kong grammar (ADR-0009) is real, but the command bodies are stubs. Tests, the e2e golden path, and CI (STORY-01.3) need to tell three outcomes apart without scraping human-readable text: a clean run, a usage/parse error, and a command that is wired but not yet implemented. Kong's default behaviour is to call `os.Exit` itself (including for `--help`), which is hostile to in-process testing.

## Options
1. Boolean success/failure only — cannot distinguish "unimplemented stub" from "real error"; e2e would have to match on log text.
2. Let Kong own the process exit — untestable without spawning subprocesses for every case; `--help` cannot be asserted in-process.
3. A small typed exit-code contract plus error sentinels, with the process boundary isolated in a `run()` helper that returns an `int`.

## Decision
Option 3.

- `internal/cli.Run(args, stdout, stderr)` parses and dispatches, returning an error. Kong's exit hook is overridden to record the requested status instead of calling `os.Exit`, so the library never terminates the process.
- Two sentinels: `ErrNotImplemented` (returned by every STORY-01.1 stub) and `ErrHelpRequested` (Kong printed usage on `--help`/empty invocation).
- `cmd/ragctl.run()` maps errors to a stable exit-code contract:
  - `0` — success, or help printed
  - `1` — parse/usage error, or a real error returned by a command's `Run`
  - `2` — command wired but not yet implemented (stubs)
- Exit `1` covers two cases: Kong prints its own parse/usage errors, but an error surfaced from a command `Run` (e.g. `migrate control` with no control-plane URL, STORY-01.5) would otherwise be swallowed. `run()` therefore prints any non-sentinel error to stderr (`ragctl: <err>`) before returning `1`, so failures are diagnosable.
- `main` is a one-liner: `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`.

## Consequences
- The e2e golden path (build the binary, invoke each subcommand) asserts exit `2` and a recognisable line, proving the wiring without a database.
- Unit tests drive `Run`/`run` with buffers; no subprocess spawning, no `os.Exit` in tests.
- As each stub is replaced (serve → STORY-04.1, work → STORY-09.1, migrate → STORY-01.5/02.2, enroll → STORY-02.3), it stops returning `ErrNotImplemented`; the exit-2 assertions for that command are updated in the same change.
- CI can gate on exit codes rather than fragile string matching.
- Convention only; supersedes nothing.
