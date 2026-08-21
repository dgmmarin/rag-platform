# ADR-0011: The app container is a compose profile, not a default service

**Status:** Accepted · **Date:** 2026-08-21 · **Requirements:** STORY-01.2

## Context
STORY-01.2's acceptance criteria say `docker compose up` starts Postgres 16 +
pgvector, MinIO, the parsing sidecar stub, "and the app". Two facts complicate a
literal reading:

1. During normal development the app processes (`ragctl serve`, `ragctl work`)
   run on the host under mprocs (`mise run dev`), not in a container, so a
   developer gets fast rebuilds and a debugger. The compose file is the
   *infrastructure* layer the host processes talk to.
2. Right now `ragctl serve` is a STORY-01.1 stub that prints a line and exits 2
   (ADR-0010). A container running it would exit immediately, and
   `mise run up` uses `docker compose up -d --wait`, which fails when a service
   exits non-zero. That would make the whole "bring the stack up" flow red for a
   reason unrelated to the developer's work.

## Options
1. Run `serve` as a default compose service. Breaks `--wait` today because the
   stub exits 2; would need a fake long-running command to stay up — dishonest.
2. Omit the app from compose entirely. Contradicts the AC and gives no
   containerised path to run the built image locally.
3. Ship the app as a compose **profile** (`--profile app`) that builds and runs
   the real image, but is not part of the default `up`. The default `up` (and
   `mise run up`) brings infra + parser up healthy; the host runs the app via
   mprocs.

## Decision
Option 3. The `app` service is defined in `docker-compose.yml` under
`profiles: ["app"]`, built from the root `Dockerfile`, wired to Postgres/MinIO/
parser. It is excluded from the default `docker compose up` and from
`mise run up --wait` so a stubbed `serve` never blocks the stack. Once
STORY-04.1 gives `serve` a real long-running server with `/healthz`, the profile
gains a healthcheck and can be promoted to a default service if desired.

The golden path this story guarantees — infra + parser healthy, and the app
(host process) reaching Postgres+pgvector, MinIO and the parser — is covered by
the e2e test in `test/e2e/stack_e2e_test.go`.

## Consequences
- `docker compose up` / `mise run up` is fast and always green: infra + parser.
- `docker compose --profile app up` (or `mise run up-app`) exercises the built
  image against the stack for CI image-smoke and reviewers.
- No fake commands or crash-loops; the stub's honest exit code is preserved.
- When `serve` becomes real, flipping the profile into the default set is a
  one-line change plus a healthcheck.
