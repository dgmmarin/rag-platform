package jobs

import (
	"context"
	"log/slog"
)

// Reconciled is one mirror row that Reconcile moved to a terminal status.
type Reconciled struct {
	JobID      string
	RiverJobID int64
	Status     string
}

// reconcileSQL heals a jobs-mirror row whose linked River job already
// finished but which the worker middleware never finalised (a worker that
// died, or a River job cancelled/discarded out-of-band). Control-plane only:
// it touches jobs and river_job, never a tenant database (ADR-0003).
const reconcileSQL = `
update jobs j
   set status      = (case r.state when 'completed' then 'succeeded'
                                    when 'discarded' then 'failed'
                                    else 'cancelled' end)::job_status,
       finished_at = coalesce(j.finished_at, now()),
       error       = coalesce(j.error, 'reconciled from river state ' || r.state)
  from river_job r
 where r.id = j.river_job_id
   and j.status in ('queued','running')
   and r.state  in ('cancelled','discarded','completed')
returning j.id::text, j.river_job_id, j.status`

// Reconcile runs reconcileSQL and parses the rows it healed. Idempotent: a run
// that finalises nothing returns an empty slice, no error.
func (p PoolDB) Reconcile(ctx context.Context) ([]Reconciled, error) {
	rows, err := p.pool.Query(ctx, reconcileSQL)
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
	return out, rows.Err()
}

// Reconciler is a thin logging wrapper over Store.Reconcile: it runs the
// reconciliation and logs one structured event per healed row (SPEC-08 §3
// mirror drift). A nil Log is tolerated (no logging).
type Reconciler struct {
	Store Store
	Log   *slog.Logger
}

// Reconcile heals drifted mirror rows and logs one "job reconciled" event per
// row healed.
func (r Reconciler) Reconcile(ctx context.Context) ([]Reconciled, error) {
	out, err := r.Store.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	if r.Log != nil {
		for _, rec := range out {
			r.Log.Info("job reconciled",
				"event", "reconciled",
				"job_id", rec.JobID,
				"river_job_id", rec.RiverJobID,
				"status", rec.Status,
				"reason", "river terminal, mirror was not finalised")
		}
	}
	return out, nil
}
