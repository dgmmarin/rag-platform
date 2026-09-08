package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// defaultIngestPerTenantCap is the per-tenant ceiling on concurrent ingest jobs
	// (SPEC-08 §1: default 2).
	defaultIngestPerTenantCap = 2
	// limiterSnooze is how long a capped job waits before retrying — short, so a
	// tenant that drops below its cap is served promptly.
	limiterSnooze = time.Second
)

// tenantLimiter is a river.WorkerMiddleware enforcing a per-tenant concurrency cap on
// the ingest queue (SPEC-08 §1, STORY-09.5): one tenant flooding the queue with syncs
// must not occupy every worker and starve the others. OSS River has no partition
// concurrency, so the cap is a per-process in-memory counter — when a tenant is already
// at its cap, the job is SNOOZED (not blocked), which frees the worker goroutine
// immediately to fetch another tenant's job and retries the snoozed one shortly. Only
// ingest-queue jobs are capped; maintenance/platform pass through.
//
// ponytail: the cap is per worker PROCESS, so with R replicas the effective global cap
// is cap*R, not cap. That still bounds any one tenant and preserves fairness within a
// process — enough for the SPEC-08 §1 goal. Upgrade path: a shared (DB/Redis) counter
// for a strict cluster-wide cap.
type tenantLimiter struct {
	river.WorkerMiddlewareDefaults
	cap    int
	snooze time.Duration
	log    *slog.Logger

	mu      sync.Mutex
	running map[string]int
}

func newTenantLimiter(capacity int, log *slog.Logger) *tenantLimiter {
	if capacity <= 0 {
		capacity = defaultIngestPerTenantCap
	}
	if log == nil {
		log = slog.Default()
	}
	return &tenantLimiter{cap: capacity, snooze: limiterSnooze, log: log, running: map[string]int{}}
}

// Work snoozes an ingest job whose tenant is already at its cap; otherwise it runs the
// job while holding one of that tenant's slots. Being outermost (registered before the
// mirror middleware), a snooze happens BEFORE the mirror marks the job running, so a
// yielded job stays queued in the mirror.
func (l *tenantLimiter) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if job.Queue != QueueIngest {
		return doInner(ctx)
	}
	tenantID := tenantIDFromArgs(job.EncodedArgs)
	if tenantID == "" {
		return doInner(ctx)
	}
	if !l.acquire(tenantID) {
		return river.JobSnooze(l.snooze)
	}
	defer l.release(tenantID)
	return doInner(ctx)
}

func (l *tenantLimiter) acquire(tenantID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[tenantID] >= l.cap {
		return false
	}
	l.running[tenantID]++
	return true
}

func (l *tenantLimiter) release(tenantID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[tenantID] <= 1 {
		delete(l.running, tenantID)
		return
	}
	l.running[tenantID]--
}

// tenantIDFromArgs pulls tenant_id from a job's encoded args (every job kind carries it).
func tenantIDFromArgs(encoded []byte) string {
	var a struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(encoded, &a)
	return a.TenantID
}
