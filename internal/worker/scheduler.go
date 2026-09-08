package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/robfig/cron/v3"
)

const (
	// schedulerLockKey is the fixed advisory-lock key the scheduler serialises on, so
	// exactly one worker replica runs any given sweep (SPEC-08 §2 leader election).
	schedulerLockKey int64 = 0x5247_5343_4844 // "RGSCHD"
	// defaultSchedulerInterval is the sweep cadence (SPEC-08 §2: every 30s).
	defaultSchedulerInterval = 30 * time.Second
	// fullSyncEveryNRuns triggers a FULL (not incremental) sync every Nth scheduled run
	// (SPEC-08 §2). Run 0 (the first) is full.
	fullSyncEveryNRuns = 7
	// gcMinInterval bounds gc_tenant to once per active tenant per this window
	// (SPEC-08 §2: GC daily).
	gcMinInterval = 24 * time.Hour
)

// Scheduler is the leader-elected loop that enqueues cron syncs and daily GC
// (SPEC-08 §2, FR-SRC-11). It runs inside `ragctl work`; several replicas may run it,
// but a Postgres advisory lock (schedulerLockKey) means only one performs any given
// sweep, so jobs are never double-enqueued — backstopped by River's per-source /
// per-tenant uniqueness. Each sweep's enqueues and its next_run_at / counter updates
// commit in the SAME transaction as the lock, so a crash mid-sweep leaves no half-done
// schedule.
type Scheduler struct {
	pool     *pgxpool.Pool
	client   *river.Client[pgx.Tx]
	interval time.Duration
	log      *slog.Logger
	now      func() time.Time
}

// NewScheduler builds the scheduler over the control-plane pool and the worker's River
// client (used to enqueue transactionally). interval<=0 takes the 30s default.
func NewScheduler(pool *pgxpool.Pool, client *river.Client[pgx.Tx], interval time.Duration, log *slog.Logger) *Scheduler {
	if interval <= 0 {
		interval = defaultSchedulerInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{pool: pool, client: client, interval: interval, log: log, now: time.Now}
}

// Run ticks every interval and sweeps until ctx is cancelled. A sweep error is logged
// and the loop continues — a transient DB blip must not kill scheduling.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.sweep(ctx); err != nil {
				s.log.Warn("scheduler sweep failed", "err", err)
			}
		}
	}
}

// sweep takes the leader lock and, if it wins, enqueues due syncs and daily GC — all in
// one transaction. A non-leader tick returns immediately (another replica is sweeping).
func (s *Scheduler) sweep(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var leader bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, schedulerLockKey).Scan(&leader); err != nil {
		return err
	}
	if !leader {
		return nil
	}
	if err := s.syncSweep(ctx, tx); err != nil {
		return err
	}
	if err := s.gcSweep(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// syncSweep enqueues a sync_source for every active cron source whose next_run_at is
// due, then advances next_run_at (from schedule_cron) and the run counter.
func (s *Scheduler) syncSweep(ctx context.Context, tx pgx.Tx) error {
	type due struct {
		id, tenantID, cron string
		count              int
	}
	rows, err := tx.Query(ctx, `
		select id::text, tenant_id::text, schedule_cron, sync_run_count
		  from sources
		 where status = 'active' and schedule_cron is not null
		   and (next_run_at is null or next_run_at <= now())`)
	if err != nil {
		return err
	}
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenantID, &d.cron, &d.count); err != nil {
			rows.Close()
			return err
		}
		dues = append(dues, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := s.now()
	for _, d := range dues {
		next, nerr := nextRun(d.cron, now)
		if nerr != nil {
			// A bad cron can never fire; park it a day out so the sweep does not spin on
			// it every tick until an admin fixes the expression.
			s.log.Warn("scheduler: unparseable cron, parking source", "source_id", d.id, "cron", d.cron, "err", nerr)
			if _, uerr := tx.Exec(ctx, `update sources set next_run_at = now() + interval '1 day' where id = $1`, d.id); uerr != nil {
				return uerr
			}
			continue
		}
		payload, _ := json.Marshal(map[string]any{"full": fullSync(d.count), "scheduled": true})
		src := d.id
		if _, err := enqueueMirrored(ctx, s.client, tx,
			SyncSourceArgs{TenantID: d.tenantID, SourceID: d.id, Full: fullSync(d.count)},
			"sync_source", d.tenantID, &src, payload); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update sources set next_run_at = $2, last_run_at = now(), sync_run_count = sync_run_count + 1
			 where id = $1`, d.id, next); err != nil {
			return err
		}
	}
	return nil
}

// gcSweep enqueues gc_tenant once per gcMinInterval for each active tenant.
func (s *Scheduler) gcSweep(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		select t.id::text
		  from tenants t
		 where t.status = 'active'
		   and not exists (
		       select 1 from jobs j
		        where j.tenant_id = t.id and j.kind = 'gc_tenant'
		          and j.queued_at > now() - make_interval(secs => $1))`, gcMinInterval.Seconds())
	if err != nil {
		return err
	}
	var tenantIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tenantIDs = append(tenantIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range tenantIDs {
		if _, err := enqueueMirrored(ctx, s.client, tx,
			GCTenantArgs{TenantID: id}, "gc_tenant", id, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// enqueueMirrored inserts a River job and its control-plane jobs mirror row in tx,
// linked by river_job_id (the STORY-09.2 producer pattern). skipped is true when River
// collapsed it onto an already-active unique job — no mirror row is written then, since
// the active job already has one. sourceID is nil for tenant-scoped kinds.
func enqueueMirrored(ctx context.Context, client *river.Client[pgx.Tx], tx pgx.Tx, args river.JobArgs, kind, tenantID string, sourceID *string, payload json.RawMessage) (skipped bool, err error) {
	res, err := client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return false, err
	}
	if res.UniqueSkippedAsDuplicate {
		return true, nil
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err = tx.Exec(ctx, `
		insert into jobs (tenant_id, source_id, kind, status, payload, river_job_id)
		values ($1::uuid, $2::uuid, $3::job_kind, 'queued', $4, $5)`,
		tenantID, sourceID, kind, []byte(payload), res.Job.ID)
	return false, err
}

// fullSync reports whether the run at this 0-based count is a full sync (SPEC-08 §2:
// full every Nth run; run 0 is full).
func fullSync(count int) bool { return count%fullSyncEveryNRuns == 0 }

// nextRun computes the next fire time from a 5-field standard cron spec (SPEC-08 §2).
func nextRun(spec string, after time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cron %q: %w", spec, err)
	}
	return sched.Next(after), nil
}
