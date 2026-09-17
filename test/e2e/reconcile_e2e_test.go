//go:build e2e

// STORY-09.2/SPEC-08 §3 golden path: jobs.Reconciler heals a jobs-mirror row that a
// crashed or restarted worker left behind. The scenario: a River job reached a
// terminal state (cancelled) but its linked jobs mirror row is still 'running',
// because the worker that would have finalised it never got the chance (ADR-0005
// makes River authoritative; the mirror can drift behind it). This runs against the
// REAL control-plane Postgres (up via `mise run up`) and inserts both rows directly,
// standing in for that crash — no worker, no mocks.
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/rag-platform/ragctl/internal/cp/jobs"
)

func TestReconcileHealsOrphanedMirrorRow(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// River's own tables (river_job, ...) are migrated separately from the goose
	// control-plane schema (ADR-0059); apply them here since this test writes to
	// river_job directly, without a worker.Worker.
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("build river migrator: %v", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("apply river migrations: %v", err)
	}

	suffix := mustSuffix(t)
	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		"reconcile-"+suffix, "Reconcile Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// A River job that already reached a terminal state (cancelled), as if River
	// itself cancelled it while no worker was running to observe it.
	var riverID int64
	if err := pool.QueryRow(ctx, `
		insert into river_job (state, max_attempts, priority, args, kind, queue, finalized_at)
		values ('cancelled', 1, 1, '{}'::jsonb, 'sync_source', 'default', now())
		returning id`).Scan(&riverID); err != nil {
		t.Fatalf("seed river_job: %v", err)
	}

	// The jobs mirror row a producer would have written, still 'running' because the
	// worker that should have finalised it never ran (the orphan this test heals).
	var jobID string
	if err := pool.QueryRow(ctx, `
		insert into jobs (tenant_id, kind, status, queued_at, started_at, river_job_id)
		values ($1::uuid, 'sync_source', 'running', now() - interval '30 seconds', now() - interval '20 seconds', $2)
		returning id::text`, tenantID, riverID).Scan(&jobID); err != nil {
		t.Fatalf("seed jobs mirror row: %v", err)
	}

	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM jobs WHERE id = '%s'", jobID))
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM river_job WHERE id = %d", riverID))
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	recon := jobs.Reconciler{Store: jobs.FromPool(pool)}
	recs, err := recon.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var healed *jobs.Reconciled
	for i := range recs {
		if recs[i].JobID == jobID {
			healed = &recs[i]
			break
		}
	}
	if healed == nil {
		t.Fatalf("Reconcile did not report job %s among %d healed rows", jobID, len(recs))
	}
	if healed.RiverJobID != riverID {
		t.Fatalf("healed RiverJobID = %d, want %d", healed.RiverJobID, riverID)
	}
	if healed.Status != "cancelled" {
		t.Fatalf("healed Status = %q, want cancelled", healed.Status)
	}

	// The mirror row itself is now finalised: cancelled, with finished_at set.
	var status string
	var finishedAt *time.Time
	var errText *string
	if err := pool.QueryRow(ctx,
		`select status::text, finished_at, error from jobs where id = $1::uuid`, jobID).
		Scan(&status, &finishedAt, &errText); err != nil {
		t.Fatalf("read healed jobs row: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("jobs.status = %q, want cancelled", status)
	}
	if finishedAt == nil {
		t.Fatal("jobs.finished_at is NULL, want set")
	}
	if errText == nil || *errText == "" {
		t.Fatal("jobs.error is empty, want a reconciliation note")
	}

	// Idempotent: a second run finds nothing left to heal for this job.
	recs2, err := recon.Reconcile(ctx)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	for _, r := range recs2 {
		if r.JobID == jobID {
			t.Fatalf("second Reconcile re-healed already-finalised job %s", jobID)
		}
	}
}
