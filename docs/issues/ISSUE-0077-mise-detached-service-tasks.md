# ISSUE-0077: mise tasks to run serve + worker detached

**Type:** Chore · **Status:** Done · **Priority:** Low · **Traces:** NFR-MNT (developer workflow)

## Summary
The repo had only foreground host tasks for the two services — `mise run api`
(`ragctl serve`) and `mise run worker` (`ragctl work`) — plus `mise run dev`
(mprocs). There was no one-command way to start both in the background and stop
them again, so a "start and forget" local run meant managing raw processes by
hand. Also, the `worker` task never sourced `.env` (unlike `api`), so a host run
got no DB / object-store / provider config.

## Change
New tasks in `mise-tasks/` (one script per task, per repo convention):

- **`serve-bg`** — start `go run ./cmd/ragctl serve` detached in its own session
  (`setsid`); log → `.run/serve.log`, pid → `.run/serve.pid`. Refuses to
  double-start.
- **`worker-bg`** — same shape for `go run ./cmd/ragctl work`; sources `.env`.
- **`services`** — start both, then poll the API's `/healthz` (port read from
  serve's own startup-log `addr`, so it follows whatever `.env` selected).
- **`services-down`** — stop both by signalling each process group (so the
  `go run` wrapper and the compiled server it spawns both stop); clears pidfiles.

Also: `worker` (foreground) now sources `.env`, matching `api`. `.run/` added to
`.gitignore` (logs + pidfiles are local dev state).

## Notes
- `setsid` makes each service its own group leader, so `services-down` can kill
  the whole group by its leader pid — the compiled `/tmp/go-build.../exe/ragctl`
  child is stopped too, not just the `go run` wrapper.
- The health wait budgets 90 s because `go run`'s first compile can take tens of
  seconds before serve binds. The grep for the bound address is guarded with
  `|| true` so an empty log on early iterations does not trip `set -e`/pipefail.

## Verification
`mise run services` → both start, API reports healthy; `mise run services-down`
→ both stop, port closed, pidfiles removed; fresh `services` after a down starts
clean (exit 0). Verified against the live local stack.
