package worker

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/obs"
)

// SampleQueueDepths counts the jobs waiting to be worked, per queue, and records
// them on jobs_queue_depth (SPEC-10 §2/§5: the queue-depth alert). It reads
// River's own river_job table on the control-plane pool (ADR-0005/0059); River
// state is authoritative, so this is the true backlog.
//
// ponytail: it counts only the 'available' state — the ready-to-run backlog.
// Ceiling: scheduled/retryable future work is not included. Upgrade path: widen
// the state set if the alert should reflect not-yet-due work too.
func SampleQueueDepths(ctx context.Context, pool *pgxpool.Pool, m *obs.Metrics) error {
	rows, err := pool.Query(ctx, `SELECT queue, count(*) FROM river_job WHERE state = 'available' GROUP BY queue`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var queue string
		var n int64
		if err := rows.Scan(&queue, &n); err != nil {
			return err
		}
		m.SetQueueDepth(queue, int(n))
	}
	return rows.Err()
}
