package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// call records one mirrorStore invocation for assertions.
type call struct {
	op      string
	id      int64
	worker  string
	attempt int
	stats   string
	errMsg  string
}

// fakeStore records calls and can be told to fail, proving a mirror error never
// changes the job's outcome.
type fakeStore struct {
	calls  []call
	failOn string // op name to return an error from
}

func (f *fakeStore) maybeFail(op string) error {
	if f.failOn == op {
		return errors.New("mirror boom")
	}
	return nil
}
func (f *fakeStore) Running(_ context.Context, id int64, w string, a int) error {
	f.calls = append(f.calls, call{op: "running", id: id, worker: w, attempt: a})
	return f.maybeFail("running")
}
func (f *fakeStore) Succeeded(_ context.Context, id int64, s json.RawMessage) error {
	f.calls = append(f.calls, call{op: "succeeded", id: id, stats: string(s)})
	return f.maybeFail("succeeded")
}
func (f *fakeStore) Failed(_ context.Context, id int64, m string) error {
	f.calls = append(f.calls, call{op: "failed", id: id, errMsg: m})
	return f.maybeFail("failed")
}
func (f *fakeStore) Retrying(_ context.Context, id int64, m string) error {
	f.calls = append(f.calls, call{op: "retrying", id: id, errMsg: m})
	return f.maybeFail("retrying")
}
func (f *fakeStore) Cancelled(_ context.Context, id int64) error {
	f.calls = append(f.calls, call{op: "cancelled", id: id})
	return f.maybeFail("cancelled")
}

func newMW(s mirrorStore) *mirrorMiddleware {
	return &mirrorMiddleware{store: s, workerID: "w-1", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func job(id int64, attempt, maxAttempts int) *rivertype.JobRow {
	return &rivertype.JobRow{ID: id, Attempt: attempt, MaxAttempts: maxAttempts}
}

// The happy path marks running before the handler and succeeded after, carrying the
// stats the handler reported through the ctx sink.
func TestMirrorSuccessRecordsRunningThenSucceededWithStats(t *testing.T) {
	f := &fakeStore{}
	err := newMW(f).Work(context.Background(), job(42, 1, 3), func(ctx context.Context) error {
		Stats(ctx).Set(json.RawMessage(`{"docs_seen":3}`))
		return nil
	})
	if err != nil {
		t.Fatalf("Work returned %v, want nil", err)
	}
	if len(f.calls) != 2 || f.calls[0].op != "running" || f.calls[1].op != "succeeded" {
		t.Fatalf("calls = %+v, want running then succeeded", f.calls)
	}
	if f.calls[0].id != 42 || f.calls[0].worker != "w-1" || f.calls[0].attempt != 1 {
		t.Fatalf("running call = %+v", f.calls[0])
	}
	if f.calls[1].stats != `{"docs_seen":3}` {
		t.Fatalf("succeeded stats = %q, want the reported stats", f.calls[1].stats)
	}
}

// A handler that reports no stats leaves jobs.stats an empty object, never null.
func TestMirrorSuccessWithoutStatsWritesEmptyObject(t *testing.T) {
	f := &fakeStore{}
	_ = newMW(f).Work(context.Background(), job(1, 1, 3), func(context.Context) error { return nil })
	if got := f.calls[1].stats; got != "{}" {
		t.Fatalf("succeeded stats = %q, want {}", got)
	}
}

// A retryable error (attempts remain) returns the row to queued and records the
// error; job_status has no 'retrying' (SPEC-08 §3 maps retrying->queued).
func TestMirrorRetryableErrorGoesToRetrying(t *testing.T) {
	f := &fakeStore{}
	want := errors.New("transient")
	err := newMW(f).Work(context.Background(), job(7, 1, 3), func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("Work returned %v, want the handler error unchanged", err)
	}
	if f.calls[1].op != "retrying" || f.calls[1].errMsg != "transient" {
		t.Fatalf("terminal call = %+v, want retrying with the error", f.calls[1])
	}
}

// The final attempt (budget spent) marks the row failed.
func TestMirrorFinalAttemptGoesToFailed(t *testing.T) {
	f := &fakeStore{}
	err := newMW(f).Work(context.Background(), job(9, 3, 3), func(context.Context) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("want the handler error passed through")
	}
	if f.calls[1].op != "failed" {
		t.Fatalf("terminal call = %+v, want failed", f.calls[1])
	}
}

// A river.JobCancel from the handler maps to the terminal cancelled state.
func TestMirrorJobCancelGoesToCancelled(t *testing.T) {
	f := &fakeStore{}
	err := newMW(f).Work(context.Background(), job(5, 1, 3), func(context.Context) error {
		return river.JobCancel(errors.New("done early"))
	})
	if err == nil {
		t.Fatal("want the cancel error passed through")
	}
	if f.calls[1].op != "cancelled" {
		t.Fatalf("terminal call = %+v, want cancelled", f.calls[1])
	}
}

// A REMOTE cancel (STORY-09.4) reaches the middleware as the handler returning a
// context error while context.Cause carries a river.JobCancel — it must map to the
// cancelled terminal, not retrying.
func TestMirrorRemoteCancelViaContextCauseGoesToCancelled(t *testing.T) {
	f := &fakeStore{}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(river.JobCancel(errors.New("cancelled remotely")))
	_ = newMW(f).Work(ctx, job(4, 1, 3), func(context.Context) error {
		return context.Canceled // handler surfaces plain cancellation; the cause carries the intent
	})
	if f.calls[1].op != "cancelled" {
		t.Fatalf("terminal call = %+v, want cancelled for a remote cancel", f.calls[1])
	}
}

// A plain drain/hard-stop cancel (no JobCancel cause) is retryable, not cancelled.
func TestMirrorPlainContextCancelIsRetryable(t *testing.T) {
	f := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = newMW(f).Work(ctx, job(4, 1, 3), func(context.Context) error { return context.Canceled })
	if f.calls[1].op != "retrying" {
		t.Fatalf("terminal call = %+v, want retrying for a plain context cancel", f.calls[1])
	}
}

// A mirror-store failure is swallowed: the job's own outcome is unchanged.
func TestMirrorWriteFailureDoesNotChangeOutcome(t *testing.T) {
	f := &fakeStore{failOn: "succeeded"}
	if err := newMW(f).Work(context.Background(), job(1, 1, 3), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Work returned %v, want nil despite the mirror write failing", err)
	}
	f2 := &fakeStore{failOn: "running"}
	sentinel := errors.New("handler")
	if err := newMW(f2).Work(context.Background(), job(1, 1, 3), func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Work returned %v, want the handler error despite a failed running write", err)
	}
}

// clip caps an over-long error string; a short one is untouched.
func TestClipCapsErrorLength(t *testing.T) {
	long := make([]byte, maxMirroredErrLen+50)
	for i := range long {
		long[i] = 'x'
	}
	if got := clip(string(long)); len(got) != maxMirroredErrLen {
		t.Fatalf("clip len = %d, want %d", len(got), maxMirroredErrLen)
	}
	if got := clip("short"); got != "short" {
		t.Fatalf("clip(short) = %q", got)
	}
}
