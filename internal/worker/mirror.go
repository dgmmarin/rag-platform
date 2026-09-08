package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// mirrorStore applies one jobs-mirror-row state transition keyed by the River job
// id (STORY-09.2, SPEC-08 §3, ADR-0060). It is the seam that lets the middleware's
// transition logic be unit-tested without a database; jobsMirror is the
// control-plane SQL implementation. Every method is a no-op for a River job with no
// mirror row — the UPDATE simply matches nothing — so a job enqueued outside a
// producer (an e2e's direct Insert, a future scheduler-only kind) never errors.
type mirrorStore interface {
	// Running stamps status=running, started_at, worker_id, attempt.
	Running(ctx context.Context, riverJobID int64, workerID string, attempt int) error
	// Succeeded stamps status=succeeded, finished_at, stats.
	Succeeded(ctx context.Context, riverJobID int64, stats json.RawMessage) error
	// Failed stamps status=failed, finished_at, error (the retry budget is spent).
	Failed(ctx context.Context, riverJobID int64, errMsg string) error
	// Retrying returns status to queued and records the error: a retry is pending,
	// and job_status has no 'retrying' (SPEC-08 §3 maps retrying->queued).
	Retrying(ctx context.Context, riverJobID int64, errMsg string) error
	// Cancelled stamps status=cancelled, finished_at (the handler returned
	// river.JobCancel — the terminal cancel mapping this story owns; the running-job
	// cancel SIGNAL is STORY-09.4).
	Cancelled(ctx context.Context, riverJobID int64) error
}

// maxMirroredErrLen caps the error text copied into jobs.error. ponytail: a blunt
// byte cap; handlers keep credentials/content out of their error strings (C-4), so
// this only guards row size, not disclosure. Upgrade path is a structured error.
const maxMirroredErrLen = 1000

// mirrorMiddleware is a river.WorkerMiddleware that mirrors River job execution into
// the control-plane jobs table (ADR-0005: River is authoritative, jobs is the mirror
// the admin UI reads). It wraps every worked job — status=running before, one
// terminal transition after, chosen from the handler's error and the attempt budget.
// It NEVER alters the error it returns to River: the mirror is a side view, not the
// queue's authority.
type mirrorMiddleware struct {
	river.WorkerMiddlewareDefaults
	store    mirrorStore
	workerID string
	log      *slog.Logger
}

// Work implements rivertype.WorkerMiddleware.
func (m *mirrorMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	m.write(ctx, func(dctx context.Context) error {
		return m.store.Running(dctx, job.ID, m.workerID, job.Attempt)
	})

	sink := &StatsSink{}
	err := doInner(withStats(ctx, sink))

	switch {
	case err == nil:
		m.write(ctx, func(dctx context.Context) error { return m.store.Succeeded(dctx, job.ID, sink.get()) })
	case isJobCancel(err) || isJobCancel(context.Cause(ctx)):
		// The handler returned river.JobCancel, OR the job was cancelled REMOTELY
		// (STORY-09.4): River cancels the work context with an ErrJobCancelledRemotely
		// cause, so the handler surfaces context.Canceled while context.Cause carries the
		// cancel. Either way the terminal state is cancelled (SPEC-08 §3/§4). A plain
		// drain/hard-stop cancel has a different cause and falls through to retrying.
		m.write(ctx, func(dctx context.Context) error { return m.store.Cancelled(dctx, job.ID) })
	case job.Attempt >= job.MaxAttempts:
		m.write(ctx, func(dctx context.Context) error { return m.store.Failed(dctx, job.ID, clip(err.Error())) })
	default:
		m.write(ctx, func(dctx context.Context) error { return m.store.Retrying(dctx, job.ID, clip(err.Error())) })
	}
	return err
}

// write runs a mirror update on a short DETACHED context, so a terminal transition
// is still recorded when the job's own ctx is already cancelled (a graceful drain,
// or a cooperative cancel). A mirror failure is logged, never propagated — it must
// not change the job's outcome (ADR-0060).
func (m *mirrorMiddleware) write(ctx context.Context, fn func(context.Context) error) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := fn(dctx); err != nil {
		m.log.Warn("job mirror write failed", "err", err)
	}
}

// isJobCancel reports whether err is (or wraps) river.JobCancel — the signal River
// uses to move a job to the cancelled state.
func isJobCancel(err error) bool {
	var jc *river.JobCancelError
	return errors.As(err, &jc)
}

func clip(s string) string {
	if len(s) > maxMirroredErrLen {
		return s[:maxMirroredErrLen]
	}
	return s
}

// --- stats sink -------------------------------------------------------------

type statsKey struct{}

// StatsSink collects a handler's job statistics for the jobs.stats mirror
// (SPEC-08 §3: stats are mirrored). The mirror middleware puts one in the job
// context; a handler that produces stats (e.g. sync_source's docs_seen/changed,
// chunks_written) calls Stats(ctx).Set(...) before returning, and the middleware
// writes it on success. A handler that reports nothing leaves stats as `{}`.
type StatsSink struct {
	mu   sync.Mutex
	data json.RawMessage
}

func withStats(ctx context.Context, s *StatsSink) context.Context {
	return context.WithValue(ctx, statsKey{}, s)
}

// Stats returns the running job's stats sink, or nil when the caller is not under
// the mirror middleware (e.g. a direct-Insert test job). A nil sink's Set is a
// no-op, so handlers can call Stats(ctx).Set(...) unconditionally.
func Stats(ctx context.Context) *StatsSink {
	s, _ := ctx.Value(statsKey{}).(*StatsSink)
	return s
}

// Set records the job's stats as JSON (last write wins). Safe on a nil sink.
func (s *StatsSink) Set(stats json.RawMessage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.data = stats
	s.mu.Unlock()
}

// get returns the recorded stats, or `{}` when the handler set nothing.
func (s *StatsSink) get() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data) == 0 {
		return json.RawMessage(`{}`)
	}
	return s.data
}
