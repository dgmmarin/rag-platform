# Local Observability Stack + Job-Action Visibility + Mirror Reconciliation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Heal jobs-mirror drift from River, log every job transition, and add an opt-in local Prometheus+Grafana+Loki+Promtail stack.

**Architecture:** A control-plane reconciler finalises mirror rows whose River job is already terminal (run on worker startup + as a periodic job). The worker/cp services emit uniform structured lifecycle events. A `deploy/obs/` compose profile scrapes the existing `/metrics` endpoints and tails the `.run/*.log` files into Loki, visualised in Grafana.

**Tech Stack:** Go 1.22, River v0.15.0 (pgx), pgxpool, Postgres (control plane), Docker Compose, Prometheus, Grafana, Loki, Promtail, mise.

**Spec:** docs/superpowers/specs/2026-09-17-observability-and-job-cancel-design.md

## Global Constraints

- Control-plane only: the reconciler and lifecycle events read/write `jobs` and `river_job` in the control-plane DB via `*pgxpool.Pool`. Never open or touch a tenant DB (ADR-0003).
- Logs and metrics carry ids and counts only — never embedded text, document content, or secrets (C-3/C-4). `stats` is the existing `sink.Stats` shape.
- The `obs` stack is local-dev only and opt-in behind a compose `profiles: ["obs"]`; a plain `mise run up` must be unaffected.
- River job correlation id across logs and the mirror is `river_job_id` (the `jobs` table has that column). The mirror uuid `job_id` is included only where already in hand (cp services), never via a new hot-path DB lookup in the worker.
- Human-facing text (README, dashboard titles, log messages) in Simplified Technical English.
- Reconcile status mapping is fixed: River `completed`→`succeeded`, `discarded`→`failed`, `cancelled`→`cancelled`.
- Commit messages: no `Co-authored-by`/`Signed-off-by` trailers.

---

### Task 1: Mirror reconciler core

**Files:**
- Create: `internal/cp/jobs/reconcile.go`
- Test: `internal/cp/jobs/reconcile_test.go`

**Interfaces:**
- Consumes: `*pgxpool.Pool` (control-plane pool; same one `FromPool` takes).
- Produces:
  - `type Reconciler struct { Pool *pgxpool.Pool; Log *slog.Logger }`
  - `type Reconciled struct { JobID string; RiverJobID int64; Status string }`
  - `func (r Reconciler) Reconcile(ctx context.Context) ([]Reconciled, error)` — runs the update, returns the rows it healed, logs one `job reconciled` event per row (fields: `event=reconciled`, `job_id`, `river_job_id`, `status`, `reason`). A nil `Log` is tolerated (no logging).

- [ ] **Step 1: Write the failing test**

Use the existing `internal/cp/jobs` test harness (a real control-plane pool from the e2e/integration seam is NOT available in unit tests, so drive the SQL through a `pgxmock` or the existing fake). If the package already tests SQL against a fake pool, mirror it; otherwise write the test against `pgxmock.NewPool()`:

```go
func TestReconcileFinalisesMirrorForTerminalRiverJob(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery("update jobs").
		WillReturnRows(pgxmock.NewRows([]string{"id", "river_job_id", "status"}).
			AddRow("11111111-1111-1111-1111-111111111111", int64(84), "cancelled"))
	r := Reconciler{Pool: mock} // NOTE: see Step 3 on the pool interface
	got, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(got) != 1 || got[0].RiverJobID != 84 || got[0].Status != "cancelled" {
		t.Fatalf("got %+v, want one cancelled row for river 84", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
```

If `pgxmock` is not already a dependency, do NOT add it: instead define `Reconciler` over the same minimal query interface `PoolDB` uses in `pool.go` and write the test with the package's existing fake. Read `internal/cp/jobs/pool.go` and `service_test.go` FIRST and follow whichever pattern is already there. The binding requirement is: the test proves the returned rows are parsed from the query result, not that a specific mock library is used.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cp/jobs/ -run TestReconcile -v`
Expected: FAIL (Reconciler undefined).

- [ ] **Step 3: Write the reconciler**

```go
package jobs

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// querier is the read/exec surface Reconcile needs; *pgxpool.Pool and the package
// fakes both satisfy it (matches pool.go's existing seam).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reconciled is one mirror row that Reconcile moved to a terminal status.
type Reconciled struct {
	JobID      string
	RiverJobID int64
	Status     string
}

// Reconciler finalises jobs-mirror rows whose linked River job is already
// terminal but which the worker middleware never got to finalise (an orphan
// whose worker died, cancelled/discarded out-of-band). It closes SPEC-08 §3
// mirror drift. Control-plane only.
type Reconciler struct {
	Pool querier
	Log  *slog.Logger
}

const reconcileSQL = `
update jobs j
   set status      = case r.state when 'completed' then 'succeeded'
                                   when 'discarded' then 'failed'
                                   else 'cancelled' end,
       finished_at = coalesce(j.finished_at, now()),
       error       = coalesce(j.error, 'reconciled from river state ' || r.state)
  from river_job r
 where r.id = j.river_job_id
   and j.status in ('queued','running')
   and r.state  in ('cancelled','discarded','completed')
returning j.id::text, j.river_job_id, j.status`

// Reconcile heals every drifted mirror row in one statement and returns them.
// Idempotent: a run that finalises nothing returns an empty slice, no error.
func (r Reconciler) Reconcile(ctx context.Context) ([]Reconciled, error) {
	rows, err := r.Pool.Query(ctx, reconcileSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Reconciled
	for rows.Next() {
		var rec Reconciled
		if err := rows.Scan(&rec.JobID, &rec.RiverJobID, &rec.Status); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, rec := range out {
		if r.Log != nil {
			r.Log.Info("job reconciled",
				"event", "reconciled", "job_id", rec.JobID,
				"river_job_id", rec.RiverJobID, "status", rec.Status,
				"reason", "river terminal, mirror was not finalised")
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cp/jobs/ -run TestReconcile -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cp/jobs/reconcile.go internal/cp/jobs/reconcile_test.go
git commit -m "jobs: reconciler heals mirror rows for terminal River jobs"
```

---

### Task 2: Wire the reconciler into the worker (startup + periodic)

**Files:**
- Create: `internal/worker/reconcile.go`
- Modify: `internal/worker/worker.go` (register worker, add periodic job, call once at startup)
- Test: `internal/worker/reconcile_test.go`

**Interfaces:**
- Consumes: `jobs.Reconciler` (Task 1); the worker's control-plane `deps.Pool` (`*pgxpool.Pool`); River `PeriodicJob` API (v0.15.0).
- Produces: `type ReconcileJobsArgs struct{}` with `Kind() = "reconcile_jobs"`; a `reconcileWorker` running on the `maintenance` queue.

- [ ] **Step 1: Write the failing test**

```go
func TestReconcileWorkerRunsReconciler(t *testing.T) {
	// A reconcileWorker.Work calls jobs.Reconciler.Reconcile and returns its error.
	// Drive it with a fake pool that returns zero rows; assert no error and that
	// Reconcile was invoked exactly once.
	// (Follow internal/worker's existing worker unit-test style: construct the
	//  worker with a fake and call Work directly.)
}
```

Read `internal/worker/worker.go` (the `gcWorker` / `todoWorker` shapes) and one existing `*_test.go` in the package first, then write the concrete test against that pattern: a `reconcileWorker{recon: <fake>}` whose `Work` returns nil on an empty reconcile.

- [ ] **Step 2: Run it**

Run: `go test ./internal/worker/ -run TestReconcile -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement the worker + args**

```go
package worker

import (
	"context"

	"github.com/riverqueue/river"

	"github.com/rag-platform/ragctl/internal/cp/jobs"
)

// ReconcileJobsArgs triggers one mirror-reconciliation pass. It carries no data:
// the reconciler operates over the whole control-plane jobs table.
type ReconcileJobsArgs struct{}

func (ReconcileJobsArgs) Kind() string { return "reconcile_jobs" }

// reconcileWorker runs the control-plane jobs reconciler (SPEC-08 §3). Registered
// on the maintenance queue so it never competes with ingest/sync work.
type reconcileWorker struct {
	river.WorkerDefaults[ReconcileJobsArgs]
	recon jobs.Reconciler
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileJobsArgs]) error {
	_, err := w.recon.Reconcile(ctx)
	return err
}
```

In `internal/worker/worker.go` `New(deps)`:
1. Build `recon := jobs.Reconciler{Pool: deps.Pool, Log: log}`.
2. `river.AddWorker(workers, &reconcileWorker{recon: recon})`.
3. Add a periodic job to the `river.Config` (alongside the existing config near line 205). If the config has no `PeriodicJobs` yet, add:

```go
PeriodicJobs: []*river.PeriodicJob{
	river.NewPeriodicJob(
		river.PeriodicInterval(60*time.Second),
		func() (river.JobArgs, *river.InsertOpts) {
			return ReconcileJobsArgs{}, &river.InsertOpts{
				Queue:  "maintenance",
				UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStateRunning}},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	),
},
```

4. After `w.client.Start(ctx)` succeeds (near line 248), run one synchronous reconcile so a restart heals prior-crash drift immediately, logging the count:

```go
if healed, err := recon.Reconcile(ctx); err != nil {
	log.Warn("startup reconcile failed", "err", err.Error())
} else if len(healed) > 0 {
	log.Info("startup reconcile healed drifted jobs", "count", len(healed))
}
```

Keep the reconciler field on the `Worker` struct if `Start` is separate from `New`; wire so the startup pass runs once when the client starts. Read the surrounding code and place the call where the control pool and `log` are in scope.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/worker/ -run TestReconcile -v && go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/reconcile.go internal/worker/reconcile_test.go internal/worker/worker.go
git commit -m "worker: run mirror reconciler on startup and every 60s (maintenance)"
```

---

### Task 3: Enrich worker lifecycle logging

**Files:**
- Modify: `internal/worker/logging.go`
- Modify: `internal/cp/jobs/jobs.go` (delete the stale "nil until EPIC-09" Canceller comment; state it is wired)
- Test: `internal/worker/logging_test.go`

**Interfaces:**
- Consumes: `rivertype.JobRow`, `context.Canceled`.
- Produces: three terminal events from the middleware — `event=finished` (ok), `event=failed` (non-cancel error), `event=cancelled` (`errors.Is(err, context.Canceled)`) — each with `kind`, `river_job_id`, `attempt`, `max_attempts`, `tenant_id`, `source_id`, `duration_ms`, and `status`. The start event gains `event=started`.

- [ ] **Step 1: Write the failing test**

```go
func TestLogMiddlewareLogsCancelledDistinctFromFailed(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := logMiddleware{log: log}
	job := &rivertype.JobRow{ID: 84, Kind: "sync_source", Attempt: 1, MaxAttempts: 3, EncodedArgs: []byte(`{}`)}

	_ = m.Work(context.Background(), job, func(context.Context) error { return context.Canceled })

	out := buf.String()
	if !strings.Contains(out, `"event":"cancelled"`) {
		t.Fatalf("ctx.Canceled must log event=cancelled, got:\n%s", out)
	}
	if strings.Contains(out, `"event":"failed"`) {
		t.Fatalf("a cancel must not be logged as failed:\n%s", out)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/worker/ -run TestLogMiddleware -v`
Expected: FAIL.

- [ ] **Step 3: Rewrite the middleware body**

Replace the finish branch so it distinguishes the three outcomes and carries the full field set (keep `jobIdentity` for tenant/source). Use `river_job_id` (not `job_id`) as the key for the River id:

```go
func (m logMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	tenantID, sourceID := jobIdentity(job.EncodedArgs)
	base := []any{
		"kind", job.Kind, "river_job_id", job.ID, "attempt", job.Attempt,
		"max_attempts", job.MaxAttempts, "tenant_id", tenantID, "source_id", sourceID,
	}
	m.log.Info("job started", append([]any{"event", "started", "status", "running"}, base...)...)

	start := time.Now()
	err := doInner(ctx)
	durMs := time.Since(start).Milliseconds()
	fields := append([]any{"duration_ms", durMs}, base...)

	switch {
	case err == nil:
		m.log.Info("job finished", append([]any{"event", "finished", "status", "succeeded"}, fields...)...)
	case errors.Is(err, context.Canceled):
		m.log.Info("job cancelled", append([]any{"event", "cancelled", "status", "cancelled"}, fields...)...)
	default:
		m.log.Warn("job failed", append([]any{"event", "failed", "status", "failed", "err", err.Error()}, fields...)...)
	}
	return err
}
```

Add `"context"` and `"errors"` imports as needed. In `internal/cp/jobs/jobs.go`, replace the Canceller doc comment lines that say it "is nil until EPIC-09 wires it" with a sentence stating it is wired at `internal/cli/api_server.go` via `riverCanceller` (`internal/cli/enqueue.go`), so a running-job cancel signals River. Do not change any code there — comment only.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/worker/ -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worker/logging.go internal/worker/logging_test.go internal/cp/jobs/jobs.go
git commit -m "worker: log started/finished/failed/cancelled as distinct job events"
```

---

### Task 4: Enqueue + cancel-requested lifecycle events

**Files:**
- Modify: `internal/cp/jobs/service.go` (`Cancel` → log `cancel_requested`)
- Modify: the source/document enqueue paths that create a mirror row (log `enqueued`)
- Test: `internal/cp/jobs/service_test.go`

**Interfaces:**
- Consumes: the `*Service` and its store; a `*slog.Logger` (add a `Log *slog.Logger` field to `Service`, nil-tolerant, set at construction in `internal/cli/api_server.go`).
- Produces: `event=cancel_requested` (fields `job_id`, `river_job_id` if known, `kind`, `tenant_id`, `source_id`, `status`) from `Service.Cancel`; `event=enqueued` from the enqueue path.

- [ ] **Step 1: Write the failing test**

Read `internal/cp/jobs/service.go` `Cancel` and `service_test.go` first. Add a `Log` field to `Service` (default nil). Write:

```go
func TestCancelLogsCancelRequested(t *testing.T) {
	var buf bytes.Buffer
	svc := NewService(newFakeStore()) // returns a cancellable running job
	svc.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	_, _ = svc.Cancel(context.Background(), someTenant, someRunningJobID)
	if !strings.Contains(buf.String(), `"event":"cancel_requested"`) {
		t.Fatalf("Cancel must log cancel_requested:\n%s", buf.String())
	}
}
```

Use whatever the fake store returns for a running, cancellable job (match `service_test.go`'s existing cancel test setup).

- [ ] **Step 2: Run it**

Run: `go test ./internal/cp/jobs/ -run TestCancelLogs -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

- Add `Log *slog.Logger` to `type Service struct`. In `Cancel`, after the cancel is accepted (both the queued-immediate and running-signalled branches), emit:

```go
if s.Log != nil {
	s.Log.Info("job cancel requested",
		"event", "cancel_requested", "job_id", job.ID, "kind", job.Kind,
		"tenant_id", job.TenantID, "source_id", derefOr(job.SourceID, ""),
		"status", job.Status)
}
```

(Add a tiny `derefOr` helper or inline the nil check; `job.SourceID` is `*string`.)

- In the enqueue path (find where a `jobs` mirror row is inserted for sync/ingest — grep `insert into jobs`), log `event=enqueued` with the same identity fields after a successful insert. If enqueue lives in a store method without a logger, thread the `Service.Log` through the service-level enqueue caller rather than the store; keep the store logic pure. If the only enqueue path is `documents.Service`/`sources.Service` (not `jobs.Service`), add the `enqueued` log there using their existing loggers, matching this field set. Read the code and put the event at the service layer that already has a logger.

- Set `jobsSvc.Log = log` in `internal/cli/api_server.go` where `jobsSvc` is constructed (near line 217).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/cp/jobs/ ./internal/cli/ -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cp/jobs/service.go internal/cp/jobs/service_test.go internal/cli/api_server.go
git commit -m "jobs: log enqueued and cancel_requested lifecycle events"
```

---

### Task 5: Observability stack config + compose `obs` profile

**Files:**
- Create: `deploy/obs/prometheus.yml`
- Create: `deploy/obs/loki.yml`
- Create: `deploy/obs/promtail.yml`
- Create: `deploy/obs/grafana/provisioning/datasources/datasources.yml`
- Create: `deploy/obs/grafana/provisioning/dashboards/dashboards.yml`
- Create: `deploy/obs/grafana/dashboards/jobs.json`
- Modify: `docker-compose.yml` (add four services behind `profiles: ["obs"]`)

**Interfaces:**
- Consumes: host metrics endpoints — `serve` on `${APP_PORT:-8091}` (its `.env` addr) and worker `:9091`, reached from containers via `host.docker.internal`; the host `.run/` dir (Promtail bind-mount, read-only).
- Produces: Prometheus (`:9090`), Loki (`:3100`), Grafana (`:3000`).

- [ ] **Step 1: Prometheus config** — `deploy/obs/prometheus.yml`:

```yaml
global:
  scrape_interval: 10s
scrape_configs:
  - job_name: ragctl-serve
    static_configs:
      - targets: ["host.docker.internal:8091"]
  - job_name: ragctl-worker
    static_configs:
      - targets: ["host.docker.internal:9091"]
```

(Serve's host metrics port follows its `.env` addr, currently `:8091`; if the operator changes it, this target changes too — note it in the README, Task 6.)

- [ ] **Step 2: Loki config** — `deploy/obs/loki.yml`: a minimal single-binary filesystem config (auth_enabled false; `common` filesystem storage under `/loki`; `schema_config` with `tsdb`/`v13`; embedded ring `inmemory`). Use the upstream single-binary example for Loki 3.x verbatim, trimmed to filesystem storage.

- [ ] **Step 3: Promtail config** — `deploy/obs/promtail.yml`:

```yaml
server:
  http_listen_port: 9080
positions:
  filename: /tmp/positions.yaml
clients:
  - url: http://loki:3100/loki/api/v1/push
scrape_configs:
  - job_name: ragctl-services
    static_configs:
      - targets: [localhost]
        labels:
          job: ragctl
          __path__: /var/run-logs/*.log
    pipeline_stages:
      - json:
          expressions:
            service: service
            event: event
            kind: kind
            status: status
      - labels:
          service:
          event:
          kind:
          status:
```

The `.run/` host dir is bind-mounted to `/var/run-logs` read-only (Step 7).

- [ ] **Step 4: Grafana datasources** — `deploy/obs/grafana/provisioning/datasources/datasources.yml`:

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
  - name: Loki
    type: loki
    access: proxy
    url: http://loki:3100
```

- [ ] **Step 5: Grafana dashboard provider** — `deploy/obs/grafana/provisioning/dashboards/dashboards.yml`:

```yaml
apiVersion: 1
providers:
  - name: ragctl
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 6: Jobs dashboard** — `deploy/obs/grafana/dashboards/jobs.json`: a Grafana dashboard JSON (schemaVersion 39+) with panels:
  - Queue depth — `jobs_queue_depth` (timeseries by `queue`).
  - Job throughput — `sum by (kind) (rate(jobs_duration_seconds_count[5m]))`.
  - Job failures — `sum by (kind) (rate(jobs_failed_total[5m]))`.
  - Job duration p95 — `histogram_quantile(0.95, sum by (le,kind) (rate(jobs_duration_seconds_bucket[5m])))`.
  - Embedding reuse rate — `sum(rate(embed_chunks_reused_total[5m])) / (sum(rate(embed_chunks_reused_total[5m])) + sum(rate(ingest_chunks_total[5m])))`.
  - Job log — a Logs panel, datasource Loki, query `{job="ragctl"} | json | event != ""`.

  Write a minimal valid dashboard JSON (panels array, each with `datasource`, `targets`, `type`). Keep it small; it is provisioned read-only.

- [ ] **Step 7: Compose services** — in `docker-compose.yml`, add under `services:` (each with `profiles: ["obs"]`, so a plain `up` skips them). Follow the existing service style (named volumes, `extra_hosts` for `host.docker.internal:host-gateway`):

```yaml
  prometheus:
    image: prom/prometheus:v2.54.1
    profiles: ["obs"]
    command: ["--config.file=/etc/prometheus/prometheus.yml"]
    volumes:
      - ./deploy/obs/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports: ["9090:9090"]
    extra_hosts: ["host.docker.internal:host-gateway"]

  loki:
    image: grafana/loki:3.1.1
    profiles: ["obs"]
    command: ["-config.file=/etc/loki/loki.yml"]
    volumes:
      - ./deploy/obs/loki.yml:/etc/loki/loki.yml:ro
      - lokidata:/loki
    ports: ["3100:3100"]

  promtail:
    image: grafana/promtail:3.1.1
    profiles: ["obs"]
    command: ["-config.file=/etc/promtail/promtail.yml"]
    volumes:
      - ./deploy/obs/promtail.yml:/etc/promtail/promtail.yml:ro
      - ./.run:/var/run-logs:ro
    depends_on: [loki]

  grafana:
    image: grafana/grafana:11.2.0
    profiles: ["obs"]
    environment:
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: "Admin"
      GF_AUTH_DISABLE_LOGIN_FORM: "true"
    volumes:
      - ./deploy/obs/grafana/provisioning:/etc/grafana/provisioning:ro
      - ./deploy/obs/grafana/dashboards:/var/lib/grafana/dashboards:ro
    ports: ["3000:3000"]
    depends_on: [prometheus, loki]
```

Add `lokidata:` under the top-level `volumes:` map (next to `pgdata`/`miniodata`).

- [ ] **Step 8: Validate config**

Run: `docker compose --profile obs config >/dev/null && echo COMPOSE_OK`
Expected: `COMPOSE_OK` (compose file parses with the profile).

- [ ] **Step 9: Commit**

```bash
git add deploy/obs docker-compose.yml
git commit -m "obs: prometheus+loki+promtail+grafana behind an opt-in compose profile"
```

---

### Task 6: mise `obs` tasks + README + smoke script

**Files:**
- Create: `mise-tasks/obs`
- Create: `mise-tasks/obs-down`
- Create: `deploy/obs/README.md`
- Create: `deploy/obs/smoke.sh`

**Interfaces:**
- Consumes: `docker compose --profile obs`; the running host services (for scrape targets).
- Produces: `mise run obs`, `mise run obs-down`.

- [ ] **Step 1: `mise-tasks/obs`**

```bash
#!/usr/bin/env bash
#MISE description="Start the local observability stack (Prometheus, Loki, Promtail, Grafana)"
set -euo pipefail
docker compose --profile obs up -d --wait
echo "obs: up — Grafana http://localhost:3000  Prometheus http://localhost:9090"
echo "obs: logs come from .run/*.log — start services with 'mise run services' for them to populate"
```

- [ ] **Step 2: `mise-tasks/obs-down`**

```bash
#!/usr/bin/env bash
#MISE description="Stop the local observability stack"
set -euo pipefail
docker compose --profile obs down
```

`chmod +x mise-tasks/obs mise-tasks/obs-down`.

- [ ] **Step 3: `deploy/obs/README.md`** — Simplified Technical English: what the stack is, the run order (`mise run up` → `mise run services` → `mise run obs`), the URLs, that logs are tailed from `.run/*.log` (so the detached `services` tasks must run, not a foreground `api` that logs to the terminal), and how to change the serve scrape target if `APP_PORT` changes.

- [ ] **Step 4: `deploy/obs/smoke.sh`** — a bash smoke check (self-skips if `docker` absent): assert `curl -sf localhost:9090/-/ready`, both Prometheus targets `up` via `localhost:9090/api/v1/targets`, `curl -sf localhost:3100/ready`, `curl -sf localhost:3000/api/health`. Print PASS/FAIL per check. `chmod +x`.

- [ ] **Step 5: Verify (live, best-effort)**

Run: `mise run obs && bash deploy/obs/smoke.sh`
Expected: stack up; smoke checks pass (targets may take a scrape interval to report `up`). If Docker/stack unavailable, note it and proceed — the configs are validated in Task 5 Step 8.

- [ ] **Step 6: Commit**

```bash
git add mise-tasks/obs mise-tasks/obs-down deploy/obs/README.md deploy/obs/smoke.sh
git commit -m "obs: mise run obs/obs-down tasks, README and smoke check"
```

---

### Task 7: e2e — reconciler heals an orphaned mirror row

**Files:**
- Create: `test/e2e/reconcile_e2e_test.go` (build tag `e2e`)

**Interfaces:**
- Consumes: the live control-plane DB (via the e2e harness's control pool) and `jobs.Reconciler`.

- [ ] **Step 1: Write the test** — follow `test/e2e/document_store_e2e_test.go` for the control-pool + migrate harness. Insert a `river_job` row in state `cancelled` and a `jobs` mirror row `running` linked by `river_job_id`; run `jobs.Reconciler{Pool: pool}.Reconcile(ctx)`; assert the mirror row is now `cancelled` with `finished_at` set, and (optionally) that the `jobs_one_active_sync_per_source` index no longer blocks inserting a fresh queued row for the same source. Clean up the rows in `t.Cleanup`.

- [ ] **Step 2: Run it (live stack)**

Run: `set -a; source .env; set +a; go test -tags e2e ./test/e2e/ -run Reconcile -v`
Expected: PASS. If the stack is unreachable, report BLOCKED-on-stack with the error; do not fake a pass.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/reconcile_e2e_test.go
git commit -m "e2e: reconciler heals an orphaned running mirror row"
```

---

### Task 8: ADR + issues + spec status

**Files:**
- Create: `docs/adr/0078-jobs-mirror-reconciliation-and-observability.md` (format per `docs/adr/0077-*`)
- Create: `docs/issues/ISSUE-0078-jobs-mirror-drift-and-lifecycle-logging.md`
- Create: `docs/issues/ISSUE-0079-local-observability-stack.md`
- Modify: the spec header `Status:` → `Implemented`

- [ ] **Step 1:** Write ADR-0078: the decision to reconcile the mirror against River (rather than make every River finalisation call back into the mirror), the periodic+startup cadence, the lifecycle-event field set, and the opt-in `obs` profile that tails `.run/*.log` (choosing file-tail over container-log scraping because the services run on the host). Correct the record that the running-job Canceller was already wired; the observed symptom was mirror drift.

- [ ] **Step 2:** Write ISSUE-0078 (reconciler + lifecycle logging, Status Done) and ISSUE-0079 (obs stack, Status Done), matching the repo issue format.

- [ ] **Step 3:** Set the spec `Status:` to `Implemented (ADR-0078)`.

- [ ] **Step 4: Commit**

```bash
git add docs/adr/0078-jobs-mirror-reconciliation-and-observability.md docs/issues/ISSUE-0078-jobs-mirror-drift-and-lifecycle-logging.md docs/issues/ISSUE-0079-local-observability-stack.md docs/superpowers/specs/2026-09-17-observability-and-job-cancel-design.md
git commit -m "ADR-0078: jobs mirror reconciliation and local observability"
```

---

## Self-Review

**Spec coverage:** Part A (reconciler) → Tasks 1, 2, 7; Part B (lifecycle logging) → Tasks 3, 4; Part C (obs stack) → Tasks 5, 6; ADR/issues/status → Task 8. Every spec section maps to a task.

**Placeholder scan:** Config-heavy steps (Loki config, dashboard JSON) point at concrete upstream shapes with the exact queries/targets to use; no "add appropriate X". The two spots that say "read the existing pattern first" (Task 1 mock choice, Task 4 enqueue-log placement) are deliberate: they bind the requirement and let the implementer match the repo's real seam rather than invent one. Every code step has the code.

**Type consistency:** `jobs.Reconciler{Pool, Log}` / `Reconcile(ctx) ([]Reconciled, error)` used in Tasks 1, 2, 7. `ReconcileJobsArgs.Kind()="reconcile_jobs"` on the maintenance queue (Task 2). Lifecycle event field set (`event`, `river_job_id`/`job_id`, `kind`, `tenant_id`, `source_id`, `status`) identical across Tasks 3 and 4. Metric names (`jobs_queue_depth`, `jobs_duration_seconds`, `jobs_failed_total`, `embed_chunks_reused_total`, `ingest_chunks_total`) match the registered names in `internal/obs/metrics.go`.
