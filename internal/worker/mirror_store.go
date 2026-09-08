package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jobsMirror is the control-plane SQL mirrorStore: it drives the `jobs` table row
// linked to a River job by river_job_id (STORY-09.2 migration 00007). It runs on the
// CONTROL-PLANE pool (the jobs table is control-plane history, C-3) — never a tenant
// database. Each UPDATE is keyed by river_job_id and no-ops (0 rows) for a River job
// with no mirror row, so a job enqueued outside a producer is harmless.
type jobsMirror struct{ pool *pgxpool.Pool }

// Running guards on status <> 'cancelled' so a cancel that landed on the row while
// the job was being picked up is never overwritten. started_at is set once (coalesce)
// so a retry keeps the first start.
func (m jobsMirror) Running(ctx context.Context, id int64, workerID string, attempt int) error {
	_, err := m.pool.Exec(ctx, `
		update jobs
		   set status = 'running', started_at = coalesce(started_at, now()),
		       worker_id = $2, attempt = $3
		 where river_job_id = $1 and status <> 'cancelled'`, id, workerID, attempt)
	return err
}

func (m jobsMirror) Succeeded(ctx context.Context, id int64, stats json.RawMessage) error {
	if len(stats) == 0 {
		stats = json.RawMessage(`{}`)
	}
	_, err := m.pool.Exec(ctx, `
		update jobs
		   set status = 'succeeded', finished_at = now(), stats = $2
		 where river_job_id = $1 and status <> 'cancelled'`, id, []byte(stats))
	return err
}

func (m jobsMirror) Failed(ctx context.Context, id int64, errMsg string) error {
	_, err := m.pool.Exec(ctx, `
		update jobs
		   set status = 'failed', finished_at = now(), error = $2
		 where river_job_id = $1 and status <> 'cancelled'`, id, errMsg)
	return err
}

// Retrying returns the row to queued (job_status has no 'retrying'; SPEC-08 §3 maps
// retrying->queued) and clears finished_at so a stale finish never lingers.
func (m jobsMirror) Retrying(ctx context.Context, id int64, errMsg string) error {
	_, err := m.pool.Exec(ctx, `
		update jobs
		   set status = 'queued', error = $2, finished_at = null
		 where river_job_id = $1 and status <> 'cancelled'`, id, errMsg)
	return err
}

// Cancelled is the terminal cancel mapping (the handler returned river.JobCancel). It
// is unguarded: a cancel is the intended final state.
func (m jobsMirror) Cancelled(ctx context.Context, id int64) error {
	_, err := m.pool.Exec(ctx, `
		update jobs
		   set status = 'cancelled', finished_at = now()
		 where river_job_id = $1`, id)
	return err
}

// newWorkerID returns a stable-enough identifier for this worker process for
// jobs.worker_id (SPEC-08 §3). It is hostname#uuid so two workers on one host stay
// distinct while the row still shows where a job ran.
func newWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s#%s", host, uuid.NewString()[:8])
}
