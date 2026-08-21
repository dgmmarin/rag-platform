# ADR-0009: Kong for the CLI and platform entrypoint

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** C-2, FR-ADM-07, FR-TEN-09, NFR-MNT-01, SPEC-02 §7

## Context
The platform ships as a single Go binary, `ragctl`, whose subcommands both operate the platform (`enroll`, `migrate`, `sync`, `reindex`, `keys rotate-dek`) and start it (`serve` for the API, `work` for the job worker). STORY-01.1 requires the subcommand skeleton; STORY-01.4 requires configuration from environment variables plus an optional file. We need a CLI library that models nested subcommands, typed flags, and env/file fallback with minimal boilerplate.

## Options
1. `spf13/cobra` + `spf13/viper` — ubiquitous, but imperative wiring and a heavy viper dependency for config precedence.
2. `alecthomas/kong` — declarative: commands and flags are Go structs with tags; built-in `env:""` fallback and pluggable config resolvers.
3. Standard library `flag` — no dependency, but no nested subcommands, no env binding, much hand-rolled parsing.

## Decision
Option 2. `ragctl` is defined as a Kong grammar: each subcommand is a struct implementing a `Run(ctx)` method; flags are struct fields with `help`, `env`, and `default` tags. Global flags — `--config`, `--log-level`, `--control-plane-url` — resolve from flag → env → optional config file (Kong resolver), satisfying STORY-01.4. Starting the platform is just running a command: `ragctl serve --addr :8080` and `ragctl work --queues ingest,maintenance` boot the API and worker. Long-running commands own their lifecycle (signal handling, graceful shutdown) inside `Run`.

## Consequences
- One grammar drives operational commands and the server/worker entrypoints — no separate flag-parsing paths.
- Config precedence (flag > env > file) is declarative via tags and one resolver; no viper.
- Flags are typed and self-documenting; `ragctl --help` and per-command help come for free.
- mise tasks (`api`, `worker`, `migrate`) call these commands, so the same flags/env work in local dev and in containers.
- Adds one dependency (`kong`); acceptable and aligned with the small-interfaces preference of ADR-0002.
- Supersedes nothing; it refines the "single binary with subcommands" note in ADR-0002 and fills in SPEC-02 §7.
